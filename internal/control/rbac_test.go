package control

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestFrontendPermissionCatalogMatchesBackend(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "account-permissions.ts"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "export const permissionCatalog")
	if start < 0 {
		t.Fatal("frontend permissionCatalog declaration is missing")
	}
	text = text[start:]
	end := strings.Index(text, "];\n")
	if end < 0 {
		t.Fatal("frontend permissionCatalog terminator is missing")
	}
	text = text[:end]
	objectPattern := regexp.MustCompile(`(?s)\{([^{}]+)\}`)
	fieldPatterns := map[string]*regexp.Regexp{
		"id":    regexp.MustCompile(`\bid\s*:\s*"([^"]+)"`),
		"class": regexp.MustCompile(`\bclass\s*:\s*"([^"]+)"`),
		"scope": regexp.MustCompile(`\bscope\s*:\s*"([^"]+)"`),
	}
	type frontendDefinition struct {
		class string
		scope string
	}
	frontend := make(map[Permission]frontendDefinition)
	for _, object := range objectPattern.FindAllStringSubmatch(text, -1) {
		fields := make(map[string]string, len(fieldPatterns))
		for name, pattern := range fieldPatterns {
			match := pattern.FindStringSubmatch(object[1])
			if len(match) != 2 {
				t.Fatalf("frontend permission object is missing %s: {%s}", name, object[1])
			}
			fields[name] = match[1]
		}
		permission := Permission(fields["id"])
		if _, duplicate := frontend[permission]; duplicate {
			t.Fatalf("frontend permission %q is duplicated", permission)
		}
		frontend[permission] = frontendDefinition{class: fields["class"], scope: fields["scope"]}
	}
	if len(frontend) != len(permissionDefinitions) {
		t.Errorf("frontend permission count=%d, backend count=%d", len(frontend), len(permissionDefinitions))
	}
	for permission, definition := range permissionDefinitions {
		frontendDefinition, ok := frontend[permission]
		if !ok {
			t.Errorf("backend permission %q is missing from the frontend catalog", permission)
			continue
		}
		class := string(definition.Class)
		if definition.Class == PermissionRootSecurity {
			class = "root"
		}
		if frontendDefinition.class != class || frontendDefinition.scope != string(definition.Scope) {
			t.Errorf("permission %q frontend class/scope=%s/%s, backend=%s/%s", permission,
				frontendDefinition.class, frontendDefinition.scope, class, definition.Scope)
		}
		delete(frontend, permission)
	}
	for permission := range frontend {
		t.Errorf("frontend permission %q is missing from the backend catalog", permission)
	}
}

func TestRolePermissions(t *testing.T) {
	tests := []struct {
		role       domain.Role
		permission Permission
		want       bool
	}{
		{domain.RoleViewer, PermissionRead, true},
		{domain.RoleViewer, PermissionOperate, false},
		{domain.RoleOperator, PermissionOperate, true},
		{domain.RoleOperator, PermissionManage, false},
		{domain.RoleAdmin, PermissionManage, true},
		{domain.RoleAdmin, PermissionRootSecurity, false},
		{domain.RoleOwner, PermissionRootSecurity, true},
	}
	for _, test := range tests {
		if got := RoleAllows(test.role, test.permission); got != test.want {
			t.Errorf("RoleAllows(%q,%q)=%v, want %v", test.role, test.permission, got, test.want)
		}
	}
}

func TestExplicitBindingPermissionsOverrideRolePreset(t *testing.T) {
	binding := domain.RoleBinding{
		Role:        domain.RoleAdmin,
		ScopeType:   "instance",
		Permissions: []string{string(PermissionInstancesRead), string(PermissionBackupsCreate)},
	}
	if !bindingAllows(binding, PermissionBackupsCreate) {
		t.Fatal("explicit backup permission was not granted")
	}
	if bindingAllows(binding, PermissionRCONExecute) || bindingAllows(binding, PermissionManage) {
		t.Fatal("admin preset leaked permissions outside the explicit set")
	}
}

func TestGlobalAuditRequiresSeparateManagementPermission(t *testing.T) {
	viewer := domain.RoleBinding{Role: domain.RoleViewer, ScopeType: "global", Permissions: presetPermissions(domain.RoleViewer)}
	admin := domain.RoleBinding{Role: domain.RoleAdmin, ScopeType: "global", Permissions: presetPermissions(domain.RoleAdmin)}
	if !bindingAllows(viewer, PermissionAuditRead) || bindingAllows(viewer, PermissionAuditReadGlobal) {
		t.Fatal("viewer audit preset crossed the global audit boundary")
	}
	if !bindingAllows(admin, PermissionAuditReadGlobal) {
		t.Fatal("administrator preset did not include global audit")
	}
}
