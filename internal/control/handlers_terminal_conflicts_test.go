package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestTerminalConflictResolutionHandlerKeepsControlOutcome(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "terminal-handler.sqlite3"))
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
	nodeID, jobID := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'Node','online');
		INSERT INTO jobs(id,node_id,type,status,progress,message,payload,result,created_at,finished_at)
		VALUES(?2,?1,'discover_instances','succeeded',100,'control done','{}','{}',?3,?3)`,
		nodeID, jobID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	reportedFingerprint := strings.Repeat("a", 64)
	if _, _, err := store.RecordTerminalOutcomeConflict(ctx, job, domain.JobFailed, reportedFingerprint, "outcome_mismatch"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := NewEventBroker(store)
	server := &Server{config: Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store: store,
		events: events, agents: NewAgentHub(store, events, logger, []byte("0123456789abcdef0123456789abcdef")), logger: logger}
	defer server.agents.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+jobID+"/terminal-conflict/resolve",
		strings.NewReader(`{"action":"keep_control","reported_fingerprint":"`+reportedFingerprint+`"}`))
	request.Header.Set("Idempotency-Key", "resolve-handler-once")
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("jobID", jobID)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{User: owner}))
	response := httptest.NewRecorder()
	server.handleResolveTerminalOutcomeConflict(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"resolution":"keep_control"`) ||
		!strings.Contains(response.Body.String(), `"acknowledgement_delivered":false`) {
		t.Fatalf("resolve terminal conflict returned %d: %s", response.Code, response.Body.String())
	}
	persisted, err := store.GetJob(ctx, jobID)
	if err != nil || persisted.Status != domain.JobSucceeded || persisted.Message != "control done" {
		t.Fatalf("resolution changed immutable Control outcome: %#v err=%v", persisted, err)
	}
	var audits int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='job.terminal_conflict.resolved' AND resource_id=?1`, jobID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("resolution audits=%d err=%v", audits, err)
	}
}

func TestTerminalConflictRoutesRequireInteractiveGlobalOwner(t *testing.T) {
	server := &Server{}
	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := server.requireInteractiveSession(server.requireGlobalPermission(PermissionRootSecurity)(reached))
	ownerBinding := domain.RoleBinding{Role: domain.RoleOwner, ScopeType: "global", Permissions: presetPermissionsForScope(domain.RoleOwner, "global")}
	tests := []struct {
		name      string
		principal Principal
		want      int
	}{
		{name: "interactive owner", principal: Principal{User: domain.User{ID: "owner", Role: domain.RoleOwner}, Bindings: []domain.RoleBinding{ownerBinding}, BindingsLoaded: true}, want: http.StatusNoContent},
		{name: "owner API token", principal: Principal{User: domain.User{ID: "owner", Role: domain.RoleOwner}, Bindings: []domain.RoleBinding{ownerBinding}, BindingsLoaded: true, APITokenID: "token"}, want: http.StatusForbidden},
		{name: "administrator", principal: Principal{User: domain.User{ID: "admin", Role: domain.RoleAdmin}, Bindings: []domain.RoleBinding{{Role: domain.RoleAdmin, ScopeType: "global", Permissions: presetPermissionsForScope(domain.RoleAdmin, "global")}}, BindingsLoaded: true}, want: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/terminal-conflicts", nil)
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, test.principal))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}
