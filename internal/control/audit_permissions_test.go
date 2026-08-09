package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestAuditPermissionsKeepGlobalAndScopedReadsIndependent(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "audit-permissions.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES
		('audit-node-a','Audit node A','online'),('audit-node-b','Audit node B','online')`); err != nil {
		t.Fatal(err)
	}
	for _, event := range []domain.AuditEvent{
		{ID: "audit-a", ActorName: "actor-a", Action: "node.updated", ResourceType: "node", ResourceID: "audit-node-a", RemoteIP: "192.0.2.10", Metadata: map[string]any{}},
		{ID: "audit-b", ActorName: "actor-b", Action: "node.updated", ResourceType: "node", ResourceID: "audit-node-b", RemoteIP: "192.0.2.20", Metadata: map[string]any{}},
	} {
		if err := store.WriteAudit(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: store}
	requestAudit := func(principal Principal, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey, principal))
		response := httptest.NewRecorder()
		server.handleListAudit(response, request)
		return response
	}
	decodeItems := func(response *httptest.ResponseRecorder) []domain.AuditEvent {
		t.Helper()
		var body struct {
			Items []domain.AuditEvent `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Items
	}

	globalOnly := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{{
		Role: domain.RoleAdmin, ScopeType: "global", Permissions: []string{string(PermissionAuditReadGlobal)},
	}}}
	response := requestAudit(globalOnly, "/api/v1/audit")
	if response.Code != http.StatusOK {
		t.Fatalf("read_global-only response=%d %s", response.Code, response.Body.String())
	}
	items := decodeItems(response)
	if len(items) != 2 || items[0].RemoteIP == "" || items[1].RemoteIP == "" {
		t.Fatalf("read_global-only items=%#v", items)
	}
	response = requestAudit(globalOnly, "/api/v1/audit?resource_id=audit-node-a")
	if response.Code != http.StatusOK {
		t.Fatalf("read_global-only resource query=%d %s", response.Code, response.Body.String())
	}
	items = decodeItems(response)
	if len(items) != 1 || items[0].ResourceID != "audit-node-a" || items[0].RemoteIP == "" {
		t.Fatalf("read_global-only resource items=%#v", items)
	}

	noPermission := Principal{BindingsLoaded: true}
	if response := requestAudit(noPermission, "/api/v1/audit"); response.Code != http.StatusForbidden {
		t.Fatalf("permissionless audit response=%d, want 403: %s", response.Code, response.Body.String())
	}

	scoped := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{{
		Role: domain.RoleViewer, ScopeType: "node", NodeID: "audit-node-a",
		Permissions: []string{string(PermissionNodesRead), string(PermissionAuditRead)},
	}}}
	response = requestAudit(scoped, "/api/v1/audit")
	if response.Code != http.StatusOK {
		t.Fatalf("scoped audit response=%d %s", response.Code, response.Body.String())
	}
	items = decodeItems(response)
	if len(items) != 1 || items[0].ResourceID != "audit-node-a" || items[0].RemoteIP != "" {
		t.Fatalf("scoped audit items=%#v", items)
	}

	globalReadOnly := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{{
		Role: domain.RoleViewer, ScopeType: "global", Permissions: []string{string(PermissionAuditRead)},
	}}}
	if response := requestAudit(globalReadOnly, "/api/v1/audit"); response.Code != http.StatusForbidden {
		t.Fatalf("global audit.read response=%d, want 403: %s", response.Code, response.Body.String())
	}
}
