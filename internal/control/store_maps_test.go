package control

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestMapAssignmentQueueEnforcesNodeStateAndIdempotency(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	userID, nodeID, otherNodeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	mapID, instanceID, otherInstanceID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role) VALUES(?1,'map-owner','map-owner','Map Owner','hash','owner');
		INSERT INTO nodes(id,name,status) VALUES(?2,'map-node','online');
		INSERT INTO nodes(id,name,status) VALUES(?3,'other-node','online');
		INSERT INTO instances(id,node_id,name,status,port) VALUES(?4,?2,'map-instance','stopped',27015);
		INSERT INTO instances(id,node_id,name,status,port) VALUES(?5,?3,'other-instance','stopped',27016);
		INSERT INTO maps(id,node_id,name,aggregate_sha256,total_size,created_by) VALUES(?6,?2,'Map',?7,1,?1);
	`, userID, nodeID, otherNodeID, instanceID, otherInstanceID, mapID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}

	fingerprint := strings.Repeat("b", 64)
	first, replayed, err := store.QueueMapInstanceJob(ctx, mapID, instanceID, "map.assign", userID, "map-assign-once", fingerprint)
	if err != nil || replayed {
		t.Fatalf("queue assignment: replayed=%v err=%v", replayed, err)
	}
	replay, replayed, err := store.QueueMapInstanceJob(ctx, mapID, instanceID, "map.assign", userID, "map-assign-once", fingerprint)
	if err != nil || !replayed || replay.ID != first.ID {
		t.Fatalf("idempotent replay: first=%s replay=%s replayed=%v err=%v", first.ID, replay.ID, replayed, err)
	}
	if _, _, err := store.QueueMapInstanceJob(ctx, mapID, otherInstanceID, "map.assign", userID, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-node assignment error=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='running' WHERE id=?1`, instanceID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueMapInstanceJob(ctx, mapID, instanceID, "map.unassign", userID, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("running unassignment error=%v", err)
	}
	inventory, err := store.QueueMapInventoryJobs(ctx, nodeID)
	if err != nil || len(inventory) != 1 || inventory[0].Type != "map.inventory" {
		t.Fatalf("queue reconnect inventory: jobs=%+v err=%v", inventory, err)
	}
	repeatedInventory, err := store.QueueMapInventoryJobs(ctx, nodeID)
	if err != nil || len(repeatedInventory) != 0 {
		t.Fatalf("duplicate reconnect inventory was queued: jobs=%+v err=%v", repeatedInventory, err)
	}
}

func TestCanceledMapDeleteLeavesRecordRetryable(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	nodeID, mapID := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO nodes(id,name,status) VALUES(?1,'map-node','online');
		INSERT INTO maps(id,node_id,name,health,aggregate_sha256,total_size) VALUES(?2,?1,'Map','deleting',?3,1);
	`, nodeID, mapID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, Type: "map.delete", Status: domain.JobCanceled,
		Payload: map[string]any{"map_id": mapID}, CreatedAt: now, FinishedAt: &now}
	if err := applyJobCancellationTx(ctx, tx, job); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var health domain.MapHealth
	if err := store.db.QueryRowContext(ctx, `SELECT health FROM maps WHERE id=?1`, mapID).Scan(&health); err != nil {
		t.Fatal(err)
	}
	if health != domain.MapDegraded {
		t.Fatalf("canceled delete left map health %q", health)
	}
}
