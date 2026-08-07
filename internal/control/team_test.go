package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		{Role: domain.RoleOwner, ScopeType: "global"},
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
		{Role: domain.RoleOperator, ScopeType: "node", NodeID: "node-a"},
		{Role: domain.RoleAdmin, ScopeType: "instance", InstanceID: "instance-b"},
	}}
	instanceA := domain.Instance{ID: "instance-a", NodeID: "node-a"}
	instanceB := domain.Instance{ID: "instance-b", NodeID: "node-b"}
	if !principal.AllowsNodePermission("node-a", PermissionOperate) {
		t.Fatal("node operator binding did not grant operate")
	}
	if principal.AllowsNodePermission("node-a", PermissionManage) {
		t.Fatal("instance admin binding was incorrectly borrowed to manage another node")
	}
	if !principal.AllowsInstancePermission(instanceA, PermissionOperate) || principal.AllowsInstancePermission(instanceA, PermissionManage) {
		t.Fatal("node binding did not propagate to its instance with the same role")
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
		{Role: domain.RoleViewer, ScopeType: "global"},
		{Role: domain.RoleAdmin, ScopeType: "node", NodeID: "node-a"},
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
		{Role: domain.RoleAdmin, ScopeType: "node", NodeID: "node-a"},
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
	owner := Principal{User: domain.User{Role: domain.RoleOwner}, Bindings: []domain.RoleBinding{{Role: domain.RoleOwner, ScopeType: "global"}}}
	admin := Principal{User: domain.User{Role: domain.RoleAdmin}, Bindings: []domain.RoleBinding{{Role: domain.RoleAdmin, ScopeType: "global"}}}
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
		Role:        domain.RoleOperator,
		ScopeType:   "instance",
		InstanceID:  instanceID,
		Permissions: []string{string(PermissionBackupsCreate)},
	}}); err == nil || !strings.Contains(err.Error(), "instances.read") {
		t.Fatalf("instance write permission without instances.read was accepted: %v", err)
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
