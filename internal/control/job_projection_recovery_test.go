package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func projectionRecoveryStore(t *testing.T) (*Store, domain.Instance) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "recovery.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','offline')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "stuck", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	return store, instance
}

func recoveryServer(t *testing.T, store *Store) *Server {
	t.Helper()
	s := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(s.Close)
	return s
}

func TestProjectionRecoveryRetriesFailuresWithoutOverwritingNewWork(t *testing.T) {
	store, instance := projectionRecoveryStore(t)
	ctx := context.Background()
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "stop", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatched(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	if _, err := store.UpdateJob(ctx, job.ID, domain.JobSucceeded, 100, "done", map[string]any{terminalOutcomeFingerprintField: fingerprint}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_recovery BEFORE UPDATE ON instances
		BEGIN SELECT RAISE(ABORT,'injected projection failure'); END`); err != nil {
		t.Fatal(err)
	}
	s := recoveryServer(t, store)
	s.reconcilePendingJobProjections(ctx)
	pending, err := store.GetJob(ctx, job.ID)
	if err != nil || !jobProjectionPending(pending) {
		t.Fatalf("failed recovery lost its barrier: %#v, %v", pending, err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", ""); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), job.ID) {
		t.Fatalf("failed recovery did not preserve and identify the lock: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_recovery`); err != nil {
		t.Fatal(err)
	}
	s.reconcilePendingJobProjections(ctx)
	settled, err := store.GetJob(ctx, job.ID)
	if err != nil || jobProjectionPending(settled) || settled.Status != domain.JobSucceeded || settled.Result[terminalOutcomeFingerprintField] != fingerprint {
		t.Fatalf("retry did not preserve the terminal outcome: %#v, %v", settled, err)
	}
	current, err := store.GetInstance(ctx, instance.ID)
	if err != nil || current.Status != domain.InstanceStopped {
		t.Fatalf("stop result not projected: %#v, %v", current, err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "start", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	s.reconcilePendingJobProjections(ctx)
	current, err = store.GetInstance(ctx, instance.ID)
	if err != nil || current.Status != domain.InstanceStarting {
		t.Fatalf("old terminal outcome overwrote new work: %#v, %v", current, err)
	}
}

func TestProjectionRecoveryNeverSettlesActiveOrUnconfirmedWork(t *testing.T) {
	for _, status := range []string{"queued", "dispatched", "running", "canceled"} {
		t.Run(status, func(t *testing.T) {
			store, instance := projectionRecoveryStore(t)
			ctx := context.Background()
			// Even an obsolete/malformed projection marker must not turn active
			// work or a cancellation request into a fabricated terminal result.
			if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,result)
				VALUES('protected-job','node',?1,'stop',?2,'{"_projection_pending":true,"cancel_pending":true}')`, instance.ID, status); err != nil {
				t.Fatal(err)
			}
			s := recoveryServer(t, store)
			s.reconcilePendingJobProjections(ctx)
			job, applied, err := store.ApplyPendingJobProjection(ctx, "protected-job")
			if err != nil || applied || !jobProjectionPending(job) || string(job.Status) != status {
				t.Fatalf("recovery guessed an outcome: %#v, applied=%v, %v", job, applied, err)
			}
			if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", ""); !errors.Is(err, ErrConflict) {
				t.Fatalf("protected work lost its lock: %v", err)
			}
			if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "a2s.info", nil, "", ""); err != nil {
				t.Fatalf("write lock blocked a read probe: %v", err)
			}
			_, _, err = store.QueueInstanceJob(ctx, instance.ID, "a2s.info", nil, "", "")
			var busy *instanceBusyError
			if !errors.Is(err, ErrConflict) || errors.As(err, &busy) {
				t.Fatalf("duplicate read probe was misreported as a write lock: %v", err)
			}
		})
	}
}

func TestProjectionRecoveryDoesNotStarveAfterFailingBatch(t *testing.T) {
	store, instance := projectionRecoveryStore(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,result)
			VALUES(?1,'node','discover_instances','failed','{"_projection_pending":true}')`, fmt.Sprintf("a-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,result)
		VALUES('z-recoverable','node',?1,'install_mod','failed','{"_projection_pending":true}');
		CREATE TRIGGER fail_first_batch BEFORE UPDATE OF result ON jobs WHEN OLD.id LIKE 'a-%'
		BEGIN SELECT RAISE(ABORT,'injected projection failure'); END`, instance.ID); err != nil {
		t.Fatal(err)
	}
	s := recoveryServer(t, store)
	s.reconcilePendingJobProjections(ctx)
	s.reconcilePendingJobProjections(ctx)
	job, err := store.GetJob(ctx, "z-recoverable")
	if err != nil || jobProjectionPending(job) {
		t.Fatalf("failing first batch starved later instance: %#v, %v", job, err)
	}
	first, err := store.GetJob(ctx, "a-00")
	if err != nil || !jobProjectionPending(first) {
		t.Fatalf("permanent failure was discarded: %#v, %v", first, err)
	}
	if s.projectionRecoveryCursor != "" {
		t.Fatal("completed sweep did not wrap around for retries")
	}
}

func TestProjectionRecoveryPublishesDeletionAndInvalidatesAuthorization(t *testing.T) {
	store, instance := projectionRecoveryStore(t)
	ctx := context.Background()
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatched(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateJob(ctx, job.ID, domain.JobSucceeded, 100, "detached", nil); err != nil {
		t.Fatal(err)
	}
	s := recoveryServer(t, store)
	generation := s.events.AuthorizationGeneration()
	s.reconcilePendingJobProjections(ctx)
	if _, err := store.GetInstance(ctx, instance.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed detach was not recovered: %v", err)
	}
	if s.events.AuthorizationGeneration() <= generation {
		t.Fatal("deletion recovery did not invalidate authorization")
	}
	events, err := store.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "instance.deleted" || events[0].ResourceID != instance.ID || events[1].Type != "job.updated" {
		t.Fatalf("recovered deletion events: %#v", events)
	}
}

func TestProjectionRecoveryUsesOutboxIndex(t *testing.T) {
	store, _ := projectionRecoveryStore(t)
	rows, err := store.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT id FROM jobs WHERE id>?1
		AND status IN ('succeeded','failed','canceled')
		AND COALESCE(json_extract(result,'$.cancel_pending'),0)<>1
		AND json_extract(result,'$._projection_pending')=1 ORDER BY id LIMIT ?2`, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		found = found || strings.Contains(detail, "jobs_pending_projection_idx")
	}
	if err := rows.Err(); err != nil || !found {
		t.Fatalf("recovery must not scan every historical result: index=%v, %v", found, err)
	}
}

func TestHousekeepingRecoversLegacyInstanceLockWithoutAgent(t *testing.T) {
	store, instance := projectionRecoveryStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,result)
		VALUES('old-job','node',?1,'install_mod','failed','{"_projection_pending":true}')`, instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected historical lock to block detach, got %v", err)
	}
	s := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer s.Close()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.RunHousekeeping(runCtx) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := store.GetJob(ctx, "old-job")
		if err != nil {
			t.Fatal(err)
		}
		if !jobProjectionPending(job) {
			if job.Status != domain.JobFailed {
				t.Fatalf("recovery changed immutable outcome: %s", job.Status)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup left the historical instance lock dependent on an Agent replay")
		case <-ticker.C:
		}
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", ""); err != nil {
		t.Fatalf("recovered instance still cannot detach: %v", err)
	}
}
