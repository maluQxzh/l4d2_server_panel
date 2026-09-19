package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPluginInspectionCanOverlapInstanceWrite(t *testing.T) {
	for _, inspectionFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-first", true: "inspection-first"}[inspectionFirst], func(t *testing.T) {
			store, instance := projectionRecoveryStore(t)
			ctx := context.Background()
			actions := []string{"stop", "inspect_plugin_archive"}
			if inspectionFirst {
				actions[0], actions[1] = actions[1], actions[0]
			}
			for _, action := range actions {
				job, _, err := store.QueueInstanceJob(ctx, instance.ID, action, nil, "", action)
				if err != nil {
					t.Fatalf("queue %s: %v", action, err)
				}
				var classified, locked bool
				if err := store.db.QueryRowContext(ctx, `SELECT is_instance_write,holds_instance_write_lock FROM jobs WHERE id=?1`, job.ID).Scan(&classified, &locked); err != nil {
					t.Fatal(err)
				}
				if classified != (action == "stop") || locked != classified {
					t.Fatalf("%s classification=%v lock=%v", action, classified, locked)
				}
			}
			for _, action := range []string{"stop"} {
				if _, _, err := store.QueueInstanceJob(ctx, instance.ID, action, nil, "", "another-"+action); !errors.Is(err, ErrConflict) {
					t.Fatalf("second mutation %s was not excluded: %v", action, err)
				}
			}
			_, _, err := store.QueueInstanceJob(ctx, instance.ID, "inspect_plugin_archive", nil, "", "second-inspection")
			var busy *instanceBusyError
			if !errors.Is(err, ErrConflict) || errors.As(err, &busy) {
				t.Fatalf("duplicate inspection must be bounded and not classified as write contention: %v", err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='canceled',result='{"cancel_pending":true}' WHERE instance_id=?1 AND type='inspect_plugin_archive'`, instance.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "inspect_plugin_archive", nil, "", "after-cancel"); !errors.Is(err, ErrConflict) {
				t.Fatalf("pending cancellation released inspection slot: %v", err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET result='{}' WHERE instance_id=?1 AND type='inspect_plugin_archive'`, instance.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "inspect_plugin_archive", nil, "", "after-ack"); err != nil {
				t.Fatalf("confirmed cancellation did not release inspection slot: %v", err)
			}
		})
	}
}

func TestParallelInspectionMigrationPreservesActiveJobs(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "migration.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := filepath.Join("..", "..", "migrations", "sqlite")
	legacy := t.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() >= "019" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, entry.Name()), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "target", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "inspect_plugin_archive", map[string]any{"name": "example.zip"}, "", "inspection")
	if err != nil {
		t.Fatal(err)
	}
	var before bool
	if err := store.db.QueryRowContext(ctx, `SELECT holds_instance_write_lock FROM jobs WHERE id=?1`, job.ID).Scan(&before); err != nil || !before {
		t.Fatalf("legacy lock=%v: %v", before, err)
	}
	blocked, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "cancel-pending", Port: 27025})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,result)
		VALUES('cancel-pending','node',?1,'stop','canceled','{"cancel_pending":true}'),
		('restored','node',?1,'restore','succeeded','{}');
		INSERT INTO restore_config_projections(job_id,instance_id) VALUES('restored',?1)`, blocked.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, blocked.ID, "stop", nil, "", "blocked"); !errors.Is(err, ErrConflict) {
		t.Fatalf("migration released cancellation barrier: %v", err)
	}
	var references int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM restore_config_projections WHERE job_id='restored'`).Scan(&references); err != nil || references != 1 {
		t.Fatalf("migration lost job foreign key reference: %d %v", references, err)
	}
	preserved, replay, err := store.QueueInstanceJob(ctx, instance.ID, "inspect_plugin_archive", map[string]any{"name": "example.zip"}, "", "inspection")
	if err != nil || !replay || preserved.ID != job.ID {
		t.Fatalf("migration lost idempotency: %#v %v %v", preserved, replay, err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "stop", nil, "", "stop"); err != nil {
		t.Fatalf("inspection still blocks writes: %v", err)
	}
	if err := store.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, root); err != nil {
		t.Fatal(err)
	}
}
