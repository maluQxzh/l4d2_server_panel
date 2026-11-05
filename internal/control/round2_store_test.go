package control

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestMapPagesFilterBeforeLimitAndHydrateWithOneConnection(t *testing.T) {
	s := packageRenameStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('hidden','Hidden','online');
	WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<600)
	INSERT INTO maps(id,node_id,name,aggregate_sha256,created_at) SELECT 'hidden-'||x,'hidden','Hidden',?1,?2 FROM n;
	WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<205)
	INSERT INTO maps(id,node_id,name,aggregate_sha256,created_at) SELECT printf('map-%03d',x),'node','Visible',?1,?3 FROM n;
	INSERT INTO map_bsps(map_id,virtual_path,start_name,dictionary_present) VALUES('map-205','maps/test.bsp','test',1);
	INSERT INTO map_assignments(map_id,instance_id) VALUES('map-205','instance');`, strings.Repeat("a", 64), time.Now(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	items, next, err := s.ListMapPage(ctx, "", []string{"node"}, nil, 200)
	if err != nil || len(items) != 200 || next == "" {
		t.Fatalf("page: %d %v", len(items), err)
	}
	if items[0].ID != "map-205" || len(items[0].BSPs) != 1 || len(items[0].Assignments) != 1 || len(items[1].BSPs) != 0 {
		t.Fatal("batch hydration crossed map boundary")
	}
	cursor, err := parseMapCursor(next)
	if err != nil {
		t.Fatal(err)
	}
	tail, next, err := s.ListMapPage(ctx, "", []string{"node"}, cursor, 200)
	if err != nil || len(tail) != 5 || next != "" {
		t.Fatalf("tail: %d %v", len(tail), err)
	}
	none, _, err := s.ListMapPage(ctx, "", []string{}, nil, 200)
	if err != nil || len(none) != 0 {
		t.Fatal("empty scope exposed maps")
	}
}

func TestDeferredAdmissionRequiresCapabilityAndNodeOwnership(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status) VALUES('deferred','node','logs.search','dispatched')`); err != nil {
		t.Fatal(err)
	}
	h := NewAgentHub(s, NewEventBroker(s), slog.Default(), nil)
	c := &agentConnection{sent: map[string]struct{}{"deferred": {}}, capabilities: map[string]struct{}{}}
	h.conns["node"] = c
	e := protocol.Envelope{Type: protocol.TypeJobDeferred, JobID: "deferred"}
	if !errors.Is(h.handleEnvelope(ctx, domain.Node{ID: "node"}, e), ErrUnauthorized) {
		t.Fatal("unnegotiated deferral accepted")
	}
	c.capabilities[protocol.CapabilityBoundedJobAdmissionV1] = struct{}{}
	if err := h.handleEnvelope(ctx, domain.Node{ID: "node"}, e); err != nil {
		t.Fatal(err)
	}
	if _, sent := c.sent["deferred"]; sent || !c.deferredUntil["deferred"].After(time.Now()) {
		t.Fatal("dispatch was not released for bounded retry")
	}
	job, err := s.GetJob(ctx, "deferred")
	if err != nil || job.Status != domain.JobDispatched {
		t.Fatal("admission changed durable job state")
	}
	h.conns["other"] = &agentConnection{capabilities: map[string]struct{}{protocol.CapabilityBoundedJobAdmissionV1: {}}}
	if !errors.Is(h.handleEnvelope(ctx, domain.Node{ID: "other"}, e), ErrUnauthorized) {
		t.Fatal("cross-node deferral accepted")
	}
	if _, _, err := s.CancelJob(ctx, "deferred"); err != nil {
		t.Fatal(err)
	}
	if err := h.handleEnvelope(ctx, domain.Node{ID: "node"}, e); err != nil {
		t.Fatal(err)
	}
	job, err = s.GetJob(ctx, "deferred")
	if err != nil || job.Status != domain.JobCanceled {
		t.Fatal("late deferral revived cancellation")
	}
}

func TestHousekeepingBatchesRetainPendingCancellationAndProjection(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	_, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<600)
	INSERT INTO jobs(id,node_id,type,status,finished_at) SELECT 'old-'||x,'node','a2s.info','succeeded',?1 FROM n;
	INSERT INTO jobs(id,node_id,type,status,result,finished_at) VALUES
	('pending-cancel','node','a2s.info','canceled','{"cancel_pending":true}',?1),
	('pending-projection','node','a2s.info','succeeded','{"_projection_pending":true}',?1);
	INSERT INTO jobs(id,node_id,type,status,payload,finished_at) VALUES('secret','node','rcon','succeeded','{"command_ciphertext":"sealed"}',?1);`, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Housekeeping(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id LIKE 'old-%'`).Scan(&count)
	if count != 100 {
		t.Fatalf("batch removed %d instead of 500", 600-count)
	}
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id LIKE 'pending-%'`).Scan(&count)
	if count != 2 {
		t.Fatal("pending state deleted")
	}
	var payload string
	s.db.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE id='secret'`).Scan(&payload)
	if payload != "{}" {
		t.Fatal("old secret not cleared")
	}
	if err := s.Housekeeping(ctx); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id LIKE 'old-%'`).Scan(&count)
	if count != 0 {
		t.Fatal("remaining batch not reclaimed")
	}
	assertQueryUsesIndex(t, s, `EXPLAIN QUERY PLAN SELECT id FROM jobs WHERE `+cleanupTerminalPredicate+` AND `+backgroundJobSQLPredicate+` AND status='failed' AND finished_at<?1`, []any{old}, "jobs_cleanup_background_idx")
	assertQueryUsesIndex(t, s, `EXPLAIN QUERY PLAN SELECT id FROM jobs WHERE `+cleanupTerminalPredicate+` AND `+cleanupSecretsPredicate+` AND finished_at<?1`, []any{old}, "jobs_cleanup_secrets_idx")
}

func TestNodeDispatchAdmissionLimitKeepsJobsQueued(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<32)
	INSERT INTO jobs(id,node_id,type,status) SELECT 'active-'||x,'node','a2s.info','running' FROM n;
	INSERT INTO jobs(id,node_id,type,status) VALUES('waiting','node','logs.search','queued');`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkDispatched(ctx, "waiting"); err != nil || ok {
		t.Fatalf("limit ignored: %v %v", ok, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded' WHERE id='active-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<40)
	INSERT INTO jobs(id,node_id,type,status) SELECT 'coordinator-'||x,'node','prepared_install_queue','running' FROM n`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkDispatched(ctx, "waiting"); err != nil || !ok {
		t.Fatalf("freed slot not used: %v %v", ok, err)
	}
}
