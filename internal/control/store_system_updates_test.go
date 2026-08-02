package control

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
)

func TestSyncSystemUpdatePersistsAndAdvancesPortableJournal(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "updates.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	state := portableupdate.State{
		JobID: "210de35a-d398-4624-a85b-20f8d78fbb41", Status: "queued", Phase: "validating",
		CurrentVersion: "1.0.0", TargetVersion: "1.1.0", PackageSHA256: strings.Repeat("a", 64),
		UploadBytes: 1234, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SyncSystemUpdate(ctx, state, ""); err != nil {
		t.Fatal(err)
	}
	completed := now.Add(time.Minute)
	state.Status, state.Phase, state.Rollback = "failed", "rolling_back", "succeeded"
	state.Error, state.UpdatedAt, state.CompletedAt = "health check failed", completed, &completed
	if err := store.SyncSystemUpdate(ctx, state, ""); err != nil {
		t.Fatal(err)
	}
	var status, phase, rollback, diagnostic, packageHash string
	var uploadBytes int64
	if err := store.db.QueryRowContext(ctx, `SELECT status,phase,rollback,error,package_sha256,upload_bytes
		FROM system_update_jobs WHERE id=?1`, state.JobID).Scan(&status, &phase, &rollback, &diagnostic, &packageHash, &uploadBytes); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || phase != "rolling_back" || rollback != "succeeded" || diagnostic != "health check failed" || packageHash != state.PackageSHA256 || uploadBytes != state.UploadBytes {
		t.Fatalf("unexpected synchronized update: status=%s phase=%s rollback=%s error=%q hash=%s bytes=%d", status, phase, rollback, diagnostic, packageHash, uploadBytes)
	}
}
