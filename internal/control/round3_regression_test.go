package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestLoginAdmissionBoundsConcurrentChecksAndFailures(t *testing.T) {
	l := newLoginLimiter()
	var wg sync.WaitGroup
	finishes := make(chan func(bool), 200)
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if finish, ok := l.acquire("192.0.2.1", "Admin"); ok {
				finishes <- finish
			}
		}()
	}
	wg.Wait()
	close(finishes)
	if len(finishes) != loginMaxConcurrent {
		t.Fatalf("admitted %d checks", len(finishes))
	}
	for finish := range finishes {
		finish(false)
		finish(false)
	}
	if l.inFlight != 0 || len(l.pending) != 0 {
		t.Fatal("slots leaked")
	}
	for range loginComboLimit - loginMaxConcurrent {
		finish, ok := l.acquire("192.0.2.1", "admin")
		if !ok {
			t.Fatal("premature failure limit")
		}
		finish(false)
	}
	if _, ok := l.acquire("192.0.2.1", "ADMIN"); ok {
		t.Fatal("failure budget bypassed")
	}
	finish, ok := l.acquire("192.0.2.2", "admin")
	if !ok {
		t.Fatal("other IP incorrectly blocked")
	}
	finish(true)
	for n := range loginIPLimit {
		finish, ok := l.acquire("192.0.2.3", fmt.Sprint(n))
		if !ok {
			t.Fatal("premature IP limit")
		}
		finish(false)
	}
	if _, ok := l.acquire("192.0.2.3", "new-user"); ok {
		t.Fatal("IP budget bypassed")
	}
}

func TestLoginSaturationReturns429BeforeStoreOrHash(t *testing.T) {
	l := newLoginLimiter()
	for range loginMaxConcurrent {
		finish, ok := l.acquire("192.0.2.1", "admin")
		if !ok {
			t.Fatal("slot missing")
		}
		defer finish(true)
	}
	s := &Server{limiter: l}
	w := httptest.NewRecorder()
	s.handleLogin(w, httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"valid-password"}`)))
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
}

func TestDisconnectedNodeDBWaitDoesNotHoldFleetRegistry(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	broker := NewEventBroker(s)
	events, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	h := NewAgentHub(s, broker, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	defer h.Close()
	node, err := s.GetNode(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.Serve(node, "127.0.0.1", w, r) }))
	defer server.Close()
	dial := func() *websocket.Conn {
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := dial()
	defer first.Close()
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("online event missing")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	first.Close()
	deadline := time.After(2 * time.Second)
	for {
		done := make(chan bool, 1)
		go func() { done <- h.Connected("node") }()
		select {
		case connected := <-done:
			if !connected {
				goto disconnected
			}
		case <-time.After(time.Second):
			t.Fatal("fleet registry blocked by database")
		}
		select {
		case <-deadline:
			t.Fatal("disconnect not observed")
		case <-time.After(time.Millisecond):
		}
	}
disconnected:
	second := dial()
	defer second.Close() // Upgrade may complete while per-node projection waits.
	if h.Connected("unrelated") {
		t.Fatal("unexpected node")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event.Type == "node.online" {
				goto reconnected
			}
		case <-time.After(3 * time.Second):
			t.Fatal("reconnect failed")
		}
	}
reconnected:
	current, err := s.GetNode(ctx, "node")
	if err != nil || current.Status != domain.NodeOnline || !h.Connected("node") {
		t.Fatalf("old disconnect overwrote reconnect: %v %v", current.Status, err)
	}
}

func TestDispatchCursorPassesUnsupportedHead(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for n := 0; n < 20; n++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,created_at) VALUES(?1,'node','game.delete','queued',?2)`, fmt.Sprintf("old-%02d", n), now.Add(time.Duration(n)*time.Microsecond)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,created_at) VALUES('supported','node','logs.search','queued',?1)`, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	h := NewAgentHub(s, NewEventBroker(s), slog.Default(), nil)
	ready := make(chan *websocket.Conn, 1)
	closeServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		ready <- c
		<-closeServer
	}))
	defer server.Close()
	defer close(closeServer)
	reader, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	client := &agentConnection{conn: <-ready, capabilities: map[string]struct{}{}, sent: map[string]struct{}{}, canceled: map[string]struct{}{}}
	h.conns["node"] = client
	h.Dispatch(ctx, "node")
	h.Dispatch(ctx, "node")
	reader.SetReadDeadline(time.Now().Add(time.Second))
	var env protocol.Envelope
	if err := reader.ReadJSON(&env); err != nil {
		t.Fatal(err)
	}
	if env.JobID != "supported" {
		t.Fatalf("wrong job %s", env.JobID)
	}
	for n := 0; n < 20; n++ {
		j, err := s.GetJob(ctx, fmt.Sprintf("old-%02d", n))
		if err != nil || j.Status != domain.JobQueued {
			t.Fatal("unsupported work changed")
		}
	}
	if client.dispatchAfter.ID != "" {
		t.Fatal("scan did not wrap after last page")
	}
}

func TestLateMetricsAggregateWithinAcceptedClockWindow(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	for _, at := range []time.Time{now.Add(agentObservationFutureWindow), now.Add(-agentObservationPastWindow)} {
		sample := domain.MetricSample{NodeID: "node", CPUPercent: 10, Players: 4, CollectedAt: at}
		if _, err := validateMetricSamples([]domain.MetricSample{sample}, 100, now); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveMetric(ctx, sample); err != nil {
			t.Fatal(err)
		}
		if err := s.AggregateMetrics(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.AggregateMetrics(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var count, sum int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*),sum(sample_count) FROM metrics_1m`).Scan(&count, &sum); err != nil {
		t.Fatal(err)
	}
	if count != 2 || sum != 2 {
		t.Fatalf("late samples lost or doubled: %d/%d", count, sum)
	}
}

func TestHistoryVisibilityBeforeLimitAndCursor(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<600)
	INSERT INTO audit_events(id,actor_name,action,resource_type,resource_id,created_at) SELECT 'hidden-'||x,'actor','node.updated','node','hidden',?1 FROM n;
	WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<600)
	INSERT INTO notifications(id,severity,type,title,message,resource,created_at) SELECT 'hidden-'||x,'warning','node.offline','hidden','hidden','hidden',?1 FROM n;`, now); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		id := fmt.Sprintf("visible-%d", n)
		if err := s.WriteAudit(ctx, domain.AuditEvent{ID: id, Action: "node.updated", ResourceType: "node", ResourceID: "node", CreatedAt: now.Add(-time.Hour), RemoteIP: "private"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO notifications(id,severity,type,title,message,resource,created_at) VALUES(?1,'warning','node.offline','Visible','Visible','node',?2)`, id, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	p := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "node", NodeID: "node", Permissions: []string{"nodes.read", "audit.read", "notifications.read"}}}}
	server := &Server{store: s}
	for name, handler := range map[string]http.HandlerFunc{"audit": server.handleListAudit, "notifications": server.handleListNotifications} {
		cursor := ""
		seen := map[string]bool{}
		for page := 0; page < 2; page++ {
			r := httptest.NewRequest("GET", "/api/v1/"+name+"?limit=2&cursor="+cursor, nil)
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey, p))
			w := httptest.NewRecorder()
			handler(w, r)
			var result struct {
				Items []struct {
					ID       string `json:"id"`
					RemoteIP string `json:"remote_ip"`
				} `json:"items"`
				Next string `json:"next_cursor"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || len(result.Items) == 0 {
				t.Fatalf("%s: %s", name, w.Body.String())
			}
			for _, i := range result.Items {
				if !strings.HasPrefix(i.ID, "visible-") || seen[i.ID] || i.RemoteIP != "" {
					t.Fatalf("unauthorized/duplicate/unredacted: %#v", i)
				}
				seen[i.ID] = true
			}
			cursor = result.Next
		}
		if len(seen) != 3 || cursor != "" {
			t.Fatalf("history pagination incomplete: %d", len(seen))
		}
	}
}

func TestHistorySQLMatchesResourceAuthorization(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('hidden','Hidden','online');
	 INSERT INTO instances(id,node_id,name,status,port) VALUES('hidden-instance','hidden','Hidden','stopped',27016);
	 INSERT INTO jobs(id,node_id,instance_id,type,status) VALUES('visible-job','node','instance','backup','succeeded'),('hidden-job','hidden','hidden-instance','backup','succeeded');`); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: s}
	p := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "instance", InstanceID: "instance", Permissions: []string{"instances.read", "audit.read", "notifications.read"}}}}
	scope, err := server.jobVisibilityScope(ctx, p, PermissionAuditRead)
	if err != nil {
		t.Fatal(err)
	}
	events := []domain.AuditEvent{
		{ID: "owned", ResourceType: "instance", ResourceID: "instance"},
		{ID: "job", ResourceType: "job", ResourceID: "visible-job"},
		{ID: "unknown", ResourceType: "unknown", ResourceID: "lost"},
		{ID: "metadata", ResourceType: "unknown", ResourceID: "lost", Metadata: map[string]any{"instance_id": "instance"}},
		{ID: "blocked-metadata", ResourceType: "instance", ResourceID: "hidden-instance", Metadata: map[string]any{"instance_id": "instance"}},
		{ID: "deleted", ResourceType: "instance", ResourceID: "deleted", Metadata: map[string]any{"instance_id": "instance"}},
		{ID: "node", ResourceType: "node", ResourceID: "node", Metadata: map[string]any{"instance_id": "instance"}},
	}
	want := map[string]bool{}
	for _, event := range events {
		event.Action = "test"
		if err := s.WriteAudit(ctx, event); err != nil {
			t.Fatal(err)
		}
		if server.auditEventAllowed(ctx, p, event) {
			want[event.ID] = true
		}
	}
	items, _, err := s.ListVisibleAuditPage(ctx, "", &scope, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(want) {
		t.Fatalf("SQL and authorization counts differ: %d vs %d", len(items), len(want))
	}
	for _, item := range items {
		if !want[item.ID] {
			t.Fatalf("SQL exposed %s", item.ID)
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE id='instance'`); err != nil {
		t.Fatal(err)
	}
	scope, err = server.jobVisibilityScope(ctx, p, PermissionAuditRead)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err = s.ListVisibleAuditPage(ctx, "", &scope, nil, 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("deleted ownership survived: %d %v", len(items), err)
	}
}

func TestHistoryTokenScopeAndLegacyMapJobs(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state) VALUES('reader','reader','reader','Reader','hash','viewer','active');
	INSERT INTO nodes(id,name,status) VALUES('hidden','Hidden','online');
	INSERT INTO instances(id,node_id,name,status,port) VALUES('hidden-instance','hidden','Hidden','stopped',27016);
	INSERT INTO jobs(id,node_id,type,status,payload,requested_by) VALUES
	('legacy-map','node','map.verify','succeeded','{}','reader'),
	('scoped-map','node','map.verify','succeeded','{"_scope_instance_id":"instance"}',NULL),
	('hidden-map','hidden','map.verify','succeeded','{"_scope_instance_id":"hidden-instance"}','reader');`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-map", "scoped-map", "hidden-map"} {
		if err := s.WriteAudit(ctx, domain.AuditEvent{ID: id, Action: "test", ResourceType: "job", ResourceID: id}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateNotification(ctx, "warning", "job.failed", "Task", "Task", id, nil); err != nil {
			t.Fatal(err)
		}
	}
	p := Principal{User: domain.User{ID: "reader"}, BindingsLoaded: true, APITokenID: "token", TokenScopes: []string{"read"}, TokenInstanceID: "instance", Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "global", Permissions: []string{"audit.read", "notifications.read"}}}}
	server := &Server{store: s}
	for _, permission := range []Permission{PermissionAuditRead, PermissionNotificationsRead} {
		scope, err := server.jobVisibilityScope(ctx, p, permission)
		if err != nil {
			t.Fatal(err)
		}
		if permission == PermissionAuditRead {
			items, _, err := s.ListVisibleAuditPage(ctx, "", &scope, nil, 100)
			if err != nil || len(items) != 2 {
				t.Fatalf("audit token scope: %d %v", len(items), err)
			}
			for _, item := range items {
				if !server.auditEventAllowed(ctx, p, item) {
					t.Fatal("SQL authorization mismatch")
				}
			}
		} else {
			items, _, err := s.ListVisibleNotificationPage(ctx, &scope, nil, 100)
			if err != nil || len(items) != 2 {
				t.Fatalf("notification token scope: %d %v", len(items), err)
			}
		}
	}
	p.TokenScopes = []string{"invalid"}
	scope, err := server.jobVisibilityScope(ctx, p, PermissionNotificationsRead)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := s.ListVisibleNotificationPage(ctx, &scope, nil, 100)
	if err != nil || len(items) != 0 {
		t.Fatal("invalid token scope exposed history")
	}
}
