package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestLoadedEmptyRoleBindingsSerializeExplicitlyAndFailClosed(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "empty-bindings.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state) VALUES
			('no-bindings','no-bindings','no-bindings','No bindings','hash','admin','active'),
			('empty-binding','empty-binding','empty-binding','Empty binding','hash','admin','active');
		INSERT INTO role_bindings(id,user_id,role,scope_type)
			VALUES('empty-binding-id','empty-binding','admin','global');
	`); err != nil {
		t.Fatal(err)
	}

	noBindings, err := store.TeamUser(ctx, "no-bindings")
	if err != nil {
		t.Fatal(err)
	}
	if noBindings.RoleBindings == nil || len(noBindings.RoleBindings) != 0 {
		t.Fatalf("loaded role bindings=%#v, want explicit empty slice", noBindings.RoleBindings)
	}
	encoded, err := json.Marshal(noBindings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"role_bindings":[]`) {
		t.Fatalf("empty role bindings were not explicit in JSON: %s", encoded)
	}

	emptyBindingUser, err := store.TeamUser(ctx, "empty-binding")
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyBindingUser.RoleBindings) != 1 || emptyBindingUser.RoleBindings[0].Permissions == nil ||
		len(emptyBindingUser.RoleBindings[0].Permissions) != 0 {
		t.Fatalf("loaded empty permission binding=%#v", emptyBindingUser.RoleBindings)
	}
	encoded, err = json.Marshal(emptyBindingUser)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"permissions":[]`) {
		t.Fatalf("empty binding permissions were not explicit in JSON: %s", encoded)
	}

	principal := Principal{User: domain.User{ID: "empty-binding", Role: domain.RoleAdmin}}
	if err := store.loadPrincipalBindings(ctx, &principal); err != nil {
		t.Fatal(err)
	}
	if !principal.BindingsLoaded || len(principal.Bindings) != 1 {
		t.Fatalf("principal bindings were not loaded: %#v", principal)
	}
	if principal.Allows(PermissionRead) || principal.Allows(PermissionManage) || principal.GlobalAllows(PermissionRootSecurity) {
		t.Fatal("loaded empty permission binding fell back to its role preset")
	}
}
