package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestMapVerificationReconcilesAssignments(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "maps.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	node, otherNode, mapID, instance, otherInstance, unknown := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'Node','online'),(?2,'Other','online');
		INSERT INTO instances(id,node_id,name,status,port) VALUES(?3,?1,'Server','stopped',27015),(?4,?2,'Other','stopped',27016);
		INSERT INTO maps(id,node_id,name,health,aggregate_sha256,total_size) VALUES(?5,?1,'Map','healthy',?6,1)`, node, otherNode, instance, otherInstance, mapID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name                                     string
		ids                                      any
		omit, initial, wantAssignment, wantError bool
		mutation                                 string
		unresolved                               int
		deletedMap                               bool
	}{
		{name: "recover hidden assignment", ids: []any{instance}, wantAssignment: true},
		{name: "remove stale center assignment", ids: []any{}, initial: true},
		{name: "old agent preserves assignment", omit: true, initial: true, wantAssignment: true},
		{name: "null is not an empty snapshot", ids: nil, initial: true, wantAssignment: true, wantError: true},
		{name: "cross node rejected atomically", ids: []any{instance, otherInstance}, wantError: true},
		{name: "duplicate ID rejected", ids: []any{instance, instance}, wantError: true},
		{name: "malformed ID rejected", ids: []any{"invalid"}, wantError: true},
		{name: "oversized snapshot rejected", ids: make([]any, 10001), wantError: true},
		{name: "unknown instance not recreated", ids: []any{unknown}, unresolved: 1},
		{name: "late verify cannot resurrect unassignment", ids: []any{instance}, mutation: "map.unassign"},
		{name: "late empty verify cannot erase assignment", ids: []any{}, initial: true, wantAssignment: true, mutation: "map.assign"},
		{name: "overlapping restore invalidates snapshot", ids: []any{instance}, mutation: "restore"},
		{name: "pending write invalidates snapshot", ids: []any{instance}, mutation: "pending"},
		{name: "component rollback can mutate map links", ids: []any{instance}, mutation: "install_mod"},
		{name: "old failed assignment can be recovered", ids: []any{instance}, mutation: "old_failed", wantAssignment: true},
		{name: "late verification cannot resurrect deleted map", ids: []any{instance}, deletedMap: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if _, err := store.db.ExecContext(ctx, `DELETE FROM jobs; DELETE FROM map_assignments; UPDATE maps SET health='healthy'`); err != nil {
				t.Fatal(err)
			}
			if scenario.initial {
				if _, err := store.db.ExecContext(ctx, `INSERT INTO map_assignments(map_id,instance_id,restart_required) VALUES(?1,?2,0)`, mapID, instance); err != nil {
					t.Fatal(err)
				}
			}
			created := time.Now().UTC().Add(-time.Minute)
			finished := created.Add(30 * time.Second)
			jobID := uuid.NewString()
			result := map[string]any{"map_id": mapID, "health": "healthy", "replicas": []any{map[string]any{"filesystem_id": "volume", "health": "healthy"}}}
			if !scenario.omit {
				result["assignment_instance_ids"] = scenario.ids
			}
			key := []byte("0123456789abcdef0123456789abcdef")
			original := protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100, Message: "verified", Result: cloneMap(result)}
			fingerprint, err := terminalJobUpdateFingerprint(key, jobID, original)
			if err != nil {
				t.Fatal(err)
			}
			result[jobProjectionPendingField] = true
			result[terminalOutcomeFingerprintField] = fingerprint
			raw, _ := json.Marshal(result)
			payload, _ := json.Marshal(map[string]any{"map_id": mapID})
			if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,payload,result,created_at,finished_at)
				VALUES(?1,?2,'map.verify','succeeded',?3,?4,?5,?6)`, jobID, node, string(payload), string(raw), created, finished); err != nil {
				t.Fatal(err)
			}
			if scenario.mutation != "" {
				kind, status := scenario.mutation, "succeeded"
				mutationFinished := finished.Add(time.Second)
				if kind == "old_failed" {
					kind, status, mutationFinished = "map.assign", "failed", created.Add(-time.Second)
				}
				if kind == "pending" {
					kind, status = "map.unassign", "running"
				}
				if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,created_at,finished_at)
					VALUES(?1,?2,?3,?4,?5,?6,?7,?8)`, uuid.NewString(), node, instance, kind, status, string(payload), created.Add(-time.Minute), mutationFinished); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.deletedMap {
				if _, err := store.db.ExecContext(ctx, `DELETE FROM maps WHERE id=?1`, mapID); err != nil {
					t.Fatal(err)
				}
			}
			projected, _, err := store.ApplyPendingJobProjection(ctx, jobID)
			if (err != nil) != scenario.wantError {
				t.Fatalf("projection err=%v", err)
			}
			var count int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM map_assignments WHERE map_id=?1`, mapID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != scenario.wantAssignment {
				t.Fatalf("assignment count=%d", count)
			}
			if scenario.wantError {
				return
			}
			if scenario.mutation != "" && scenario.mutation != "old_failed" && projected.Result["assignment_sync"] != "skipped_concurrent_write" {
				t.Fatalf("stale snapshot accepted: %#v", projected.Result)
			}
			if equivalent, err := terminalReplayEquivalent(key, projected, original, fingerprint); err != nil || !equivalent {
				t.Fatalf("snapshot enrichment broke terminal replay: %v %v", equivalent, err)
			}
			if scenario.unresolved > 0 {
				item, err := store.GetMap(ctx, mapID)
				if err != nil || item.Health != domain.MapDegraded || intFromAny(projected.Result["unresolved_assignment_count"]) != scenario.unresolved {
					t.Fatalf("unknown registration lost: %+v %#v %v", item, projected.Result, err)
				}
			}
			if scenario.wantAssignment {
				var pending bool
				if err := store.db.QueryRowContext(ctx, `SELECT restart_required FROM map_assignments WHERE map_id=?1 AND instance_id=?2`, mapID, instance).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if pending == scenario.initial {
					t.Fatal("recovery must mark new assignments pending, preserve existing flags")
				}
			}
			if _, applied, err := store.ApplyPendingJobProjection(ctx, jobID); err != nil || applied {
				t.Fatalf("terminal replay re-applied: %v %v", applied, err)
			}
		})
	}
}

func TestMapAssignmentProjectsPendingRefresh(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "maps.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	node, mapID, instance := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'Node','online');
		INSERT INTO instances(id,node_id,name,status,port) VALUES(?2,?1,'Server','stopped',27015);
		INSERT INTO maps(id,node_id,name,health,aggregate_sha256,total_size) VALUES(?3,?1,'Map','healthy',?4,1)`, node, instance, mapID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	job := domain.Job{NodeID: node, InstanceID: instance, Type: "map.assign", Status: domain.JobSucceeded, CreatedAt: time.Now().UTC(), Payload: map[string]any{"map_id": mapID}, Result: map[string]any{"map_id": mapID, "instance_id": instance, "restart_required": true}}
	for _, pending := range []bool{true, false} {
		job.Result["restart_required"] = pending
		if err := store.ApplyJobResult(ctx, job); err != nil {
			t.Fatal(err)
		}
		var stored bool
		if err := store.db.QueryRowContext(ctx, `SELECT restart_required FROM map_assignments WHERE map_id=?1 AND instance_id=?2`, mapID, instance).Scan(&stored); err != nil || stored != pending {
			t.Fatalf("pending=%v want %v err=%v", stored, pending, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE maps SET health='broken' WHERE id=?1`, mapID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueMapInstanceJob(ctx, mapID, instance, "map.assign", "", "", ""); err == nil {
		t.Fatal("broken map accepted new assignment")
	}
	if _, _, err := store.QueueMapInstanceJob(ctx, mapID, instance, "map.unassign", "", "", ""); err != nil {
		t.Fatalf("broken map cannot be unassigned: %v", err)
	}
}
