package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestUpdateTeamUserInvalidatesAuthorizationAfterPostCommitReloadError(t *testing.T) {
	server := &Server{events: NewEventBroker(nil)}
	before := server.events.AuthorizationGeneration()
	sentinel := errors.New("post-commit reload failed")
	called := false

	_, err := server.updateTeamUserAndInvalidate(context.Background(), "actor", "user", UpdateTeamUserInput{},
		func(context.Context, string, string, UpdateTeamUserInput) (domain.User, error) {
			called = true
			return domain.User{}, sentinel
		})
	if !called || !errors.Is(err, sentinel) {
		t.Fatalf("update result was not preserved: called=%v err=%v", called, err)
	}
	if got := server.events.AuthorizationGeneration(); got != before+1 {
		t.Fatalf("authorization generation = %d, want %d", got, before+1)
	}
}

func TestAdministratorCannotAccessAccountManagementHandlers(t *testing.T) {
	server := &Server{}
	admin := Principal{
		User:           domain.User{ID: "admin", Role: domain.RoleAdmin},
		Bindings:       []domain.RoleBinding{{Role: domain.RoleAdmin, ScopeType: "global"}},
		BindingsLoaded: true,
	}
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{name: "list users", method: http.MethodGet, path: "/api/v1/users", handler: server.handleListUsers},
		{name: "create user", method: http.MethodPost, path: "/api/v1/users", body: `{"username":"viewer","password":"long-enough-password","role":"viewer"}`, handler: server.handleCreateUser},
		{name: "read bindings", method: http.MethodGet, path: "/api/v1/users/user-id/role-bindings", handler: server.handleUserRoleBindings},
		{name: "update and password reset", method: http.MethodPatch, path: "/api/v1/users/user-id", body: `{"display_name":"Changed","password":"replacement-password"}`, handler: server.handleUpdateUser},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, admin))
			response := httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("account API returned %d, want 403: %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "owner_permission_required") {
				t.Fatalf("account API did not return the owner-only error: %s", response.Body.String())
			}
		})
	}
}

func TestAccountSecurityRequiresInteractiveGlobalOwner(t *testing.T) {
	owner := Principal{User: domain.User{Role: domain.RoleOwner}, Bindings: []domain.RoleBinding{
		{Role: domain.RoleOwner, ScopeType: "global", Permissions: presetPermissionsForScope(domain.RoleOwner, "global")},
	}, BindingsLoaded: true}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, owner))
	if _, allowed := requireAccountSecurity(httptest.NewRecorder(), request); !allowed {
		t.Fatal("global owner was rejected from account management")
	}

	owner.APITokenID = "owner-token"
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, owner))
	response := httptest.NewRecorder()
	if _, allowed := requireAccountSecurity(response, request); allowed || response.Code != http.StatusForbidden {
		t.Fatalf("owner API token crossed the root security boundary: allowed=%v status=%d", allowed, response.Code)
	}
}

func TestSessionRoleBindingsDoNotMixPrivilegesAcrossResources(t *testing.T) {
	principal := Principal{User: domain.User{Role: domain.RoleAdmin}, Bindings: []domain.RoleBinding{
		{Role: domain.RoleOperator, ScopeType: "node", NodeID: "node-a", Permissions: presetPermissionsForScope(domain.RoleOperator, "node")},
		{Role: domain.RoleAdmin, ScopeType: "instance", InstanceID: "instance-b", Permissions: presetPermissionsForScope(domain.RoleAdmin, "instance")},
	}}
	instanceA := domain.Instance{ID: "instance-a", NodeID: "node-a"}
	instanceB := domain.Instance{ID: "instance-b", NodeID: "node-b"}
	if !principal.AllowsNodePermission("node-a", PermissionOperate) {
		t.Fatal("node operator binding did not grant operate")
	}
	if principal.AllowsNodePermission("node-a", PermissionNodesUpdate) {
		t.Fatal("instance admin binding was incorrectly borrowed to manage another node")
	}
	if !principal.AllowsNodePermission("node-a", PermissionMapsImport) || principal.AllowsNodePermission("node-b", PermissionMapsImport) {
		t.Fatal("operator map management did not stay within its node binding")
	}
	if !principal.AllowsInstancePermission(instanceA, PermissionOperate) || principal.AllowsInstancePermission(instanceA, PermissionInstancesDelete) {
		t.Fatal("node binding did not propagate to its instance with the same role")
	}
	if !principal.AllowsInstancePermission(instanceA, PermissionMapsDelete) {
		t.Fatal("node operator binding did not propagate map management to its instance")
	}
	if !principal.AllowsInstancePermission(instanceB, PermissionManage) {
		t.Fatal("instance admin binding did not grant manage on its instance")
	}
	if principal.AllowsNode("node-b") {
		t.Fatal("instance binding leaked node visibility")
	}
}

func TestGlobalViewerAndNodeAdminAreCombinedAtTheMatchingScopeOnly(t *testing.T) {
	principal := Principal{User: domain.User{Role: domain.RoleAdmin}, Bindings: []domain.RoleBinding{
		{Role: domain.RoleViewer, ScopeType: "global", Permissions: presetPermissionsForScope(domain.RoleViewer, "global")},
		{Role: domain.RoleAdmin, ScopeType: "node", NodeID: "node-a", Permissions: presetPermissionsForScope(domain.RoleAdmin, "node")},
	}}
	if !principal.AllowsNodePermission("node-b", PermissionRead) {
		t.Fatal("global viewer binding did not grant read")
	}
	if principal.AllowsNodePermission("node-b", PermissionOperate) {
		t.Fatal("node-a admin privilege leaked to node-b")
	}
	if !principal.AllowsNodePermission("node-a", PermissionManage) {
		t.Fatal("node admin binding did not grant manage")
	}
	if principal.GlobalAllows(PermissionManage) {
		t.Fatal("resource admin binding granted global management")
	}
}

func TestAPITokenScopeIntersectsRoleBinding(t *testing.T) {
	principal := Principal{User: domain.User{Role: domain.RoleAdmin}, Bindings: []domain.RoleBinding{
		{Role: domain.RoleAdmin, ScopeType: "node", NodeID: "node-a", Permissions: presetPermissionsForScope(domain.RoleAdmin, "node")},
	}, APITokenID: "token", TokenScopes: []string{"operate"}, TokenNodeID: "node-a"}
	if !principal.AllowsNodePermission("node-a", PermissionOperate) {
		t.Fatal("in-scope operation was rejected")
	}
	if principal.AllowsNodePermission("node-a", PermissionManage) {
		t.Fatal("API token scope did not narrow role binding")
	}
	if principal.AllowsNodePermission("node-b", PermissionRead) {
		t.Fatal("API token resource scope was not intersected")
	}
}

func TestOwnerBindingValidation(t *testing.T) {
	if _, err := normalizeRoleBindings([]domain.RoleBinding{{Role: domain.RoleOwner, ScopeType: "node",
		NodeID: "04ca41f2-9760-47b1-91d7-acba7632ded9"}}); err == nil {
		t.Fatal("node-scoped owner was accepted")
	}
	owner := Principal{User: domain.User{Role: domain.RoleOwner}, Bindings: []domain.RoleBinding{{Role: domain.RoleOwner, ScopeType: "global", Permissions: presetPermissionsForScope(domain.RoleOwner, "global")}}}
	admin := Principal{User: domain.User{Role: domain.RoleAdmin}, Bindings: []domain.RoleBinding{{Role: domain.RoleAdmin, ScopeType: "global", Permissions: presetPermissionsForScope(domain.RoleAdmin, "global")}}}
	target := []domain.RoleBinding{{Role: domain.RoleOwner, ScopeType: "global"}}
	if !actorCanAssignBindings(owner, target) {
		t.Fatal("owner could not assign an owner binding")
	}
	if actorCanAssignBindings(admin, target) {
		t.Fatal("administrator could assign an owner binding")
	}
}

func TestLegacyRoleRequestBecomesGlobalBinding(t *testing.T) {
	bindings, err := bindingsFromRequest(domain.RoleOperator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ScopeType != "global" || bindings[0].Role != domain.RoleOperator {
		t.Fatalf("unexpected compatibility binding: %#v", bindings)
	}
}

func TestAuthenticatedPrincipalWithoutBindingsFailsClosed(t *testing.T) {
	principal := Principal{User: domain.User{Role: domain.RoleOwner}, BindingsLoaded: true}
	if principal.Allows(PermissionRead) || principal.GlobalAllows(PermissionRootSecurity) ||
		principal.AllowsNodePermission("node-a", PermissionRead) {
		t.Fatal("principal with an explicitly loaded empty binding set retained legacy role authority")
	}
}

func TestNormalizeFineGrainedBindings(t *testing.T) {
	instanceID := "04ca41f2-9760-47b1-91d7-acba7632ded9"
	bindings, err := normalizeRoleBindings([]domain.RoleBinding{{
		Role:        domain.RoleAdmin,
		ScopeType:   "instance",
		InstanceID:  instanceID,
		Permissions: []string{string(PermissionInstancesRead), string(PermissionBackupsCreate)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || !equalStringSets(bindings[0].Permissions, []string{string(PermissionBackupsCreate), string(PermissionInstancesRead)}) {
		t.Fatalf("unexpected normalized permissions: %#v", bindings)
	}
	if _, err := normalizeRoleBindings([]domain.RoleBinding{{
		Role: domain.RoleAdmin, ScopeType: "instance", InstanceID: instanceID, Permissions: make([]string, 0),
	}}); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("explicit empty permissions were expanded instead of rejected: %v", err)
	}
	preset, err := normalizeRoleBindings([]domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "instance", InstanceID: instanceID}})
	if err != nil || len(preset) != 1 || len(preset[0].Permissions) == 0 {
		t.Fatalf("omitted permissions did not receive a preset: %#v err=%v", preset, err)
	}
	if _, err := normalizeRoleBindings([]domain.RoleBinding{{
		Role:        domain.RoleOperator,
		ScopeType:   "instance",
		InstanceID:  instanceID,
		Permissions: []string{string(PermissionBackupsCreate)},
	}}); err == nil || !strings.Contains(err.Error(), "instances.read") {
		t.Fatalf("instance write permission without instances.read was accepted: %v", err)
	}
	mapPermissions := []string{
		string(PermissionInstancesRead), string(PermissionMapsAssign), string(PermissionMapsDelete),
		string(PermissionMapsImport), string(PermissionMapsRead), string(PermissionMapsVerify),
	}
	mapBinding, err := normalizeRoleBindings([]domain.RoleBinding{{
		Role: domain.RoleAdmin, ScopeType: "instance", InstanceID: instanceID, Permissions: mapPermissions,
	}})
	if err != nil || len(mapBinding) != 1 || !equalStringSets(mapBinding[0].Permissions, mapPermissions) {
		t.Fatalf("instance shared-map permissions were rejected: binding=%#v err=%v", mapBinding, err)
	}
	if _, err := normalizeRoleBindings([]domain.RoleBinding{{
		Role:        domain.RoleAdmin,
		ScopeType:   "instance",
		InstanceID:  instanceID,
		Permissions: []string{string(PermissionNodesEnroll)},
	}}); err == nil {
		t.Fatal("global permission was accepted on an instance binding")
	}
}

func TestTeamHandlersRejectExplicitEmptyOrNullPermissions(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "explicit-empty-permissions.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	owner, err = store.TeamUser(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := store.CreateTeamUser(ctx, owner.ID, CreateTeamUserInput{
		Username: "target-viewer", DisplayName: "Target viewer",
		Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "global"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{User: owner, Bindings: owner.RoleBindings, BindingsLoaded: true}
	server := &Server{store: store, events: NewEventBroker(store)}

	for _, permissions := range []string{"[]", "null"} {
		t.Run("create "+permissions, func(t *testing.T) {
			body := `{"username":"bad-admin-` + strings.Trim(permissions, "[]") + `","role_bindings":[{"role":"admin","scope_type":"global","permissions":` + permissions + `}]}`
			request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, principal))
			response := httptest.NewRecorder()
			server.handleCreateUser(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_role_bindings") {
				t.Fatalf("create response=%d %s", response.Code, response.Body.String())
			}
		})
		t.Run("update "+permissions, func(t *testing.T) {
			body := `{"role_bindings":[{"role":"admin","scope_type":"global","permissions":` + permissions + `}]}`
			request := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+target.ID, strings.NewReader(body))
			routeContext := chi.NewRouteContext()
			routeContext.URLParams.Add("userID", target.ID)
			request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, principal))
			response := httptest.NewRecorder()
			server.handleUpdateUser(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_role_bindings") {
				t.Fatalf("update response=%d %s", response.Code, response.Body.String())
			}
			unchanged, err := store.TeamUser(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.Role != domain.RoleViewer || len(unchanged.RoleBindings) != 1 ||
				!bindingAllows(unchanged.RoleBindings[0], PermissionInstancesRead) || bindingAllows(unchanged.RoleBindings[0], PermissionManage) {
				t.Fatalf("invalid update changed target authorization: %#v", unchanged)
			}
		})
	}

	for _, test := range []struct {
		name   string
		method string
		body   string
	}{
		{
			name:   "create explicit null bindings with legacy role",
			method: http.MethodPost,
			body:   `{"username":"null-bindings","role":"admin","role_bindings":null}`,
		},
		{
			name:   "create explicit empty bindings with legacy role",
			method: http.MethodPost,
			body:   `{"username":"empty-bindings","role":"admin","role_bindings":[]}`,
		},
		{
			name:   "update explicit null bindings with legacy role",
			method: http.MethodPatch,
			body:   `{"role":"admin","role_bindings":null}`,
		},
		{
			name:   "update explicit empty bindings",
			method: http.MethodPatch,
			body:   `{"role_bindings":[]}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/api/v1/users/"+target.ID, strings.NewReader(test.body))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, principal))
			response := httptest.NewRecorder()
			if test.method == http.MethodPost {
				server.handleCreateUser(response, request)
			} else {
				routeContext := chi.NewRouteContext()
				routeContext.URLParams.Add("userID", target.ID)
				request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
				server.handleUpdateUser(response, request)
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_role_bindings") {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
		})
	}

	t.Run("omitted bindings keep legacy role fallback", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(
			`{"username":"legacy-role-viewer","display_name":"Legacy role viewer","role":"viewer"}`))
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey, principal))
		response := httptest.NewRecorder()
		server.handleCreateUser(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create response=%d %s", response.Code, response.Body.String())
		}
		var body struct {
			User domain.User `json:"user"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.User.RoleBindings) != 1 || len(body.User.RoleBindings[0].Permissions) == 0 ||
			!bindingAllows(body.User.RoleBindings[0], PermissionInstancesRead) {
			t.Fatalf("legacy role did not receive its preset: %#v", body.User.RoleBindings)
		}
	})
}
