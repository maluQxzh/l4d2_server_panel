package control

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestBackupManagedPackagePathsExcludeLocalScansAndDeleteProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "backup-delete.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "backup", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,file_count) VALUES
		('managed-package',?1,'Managed','plugin','upload',?2,'left4dead2/addons/sourcemod/plugins/Managed.smx',1,'normal',2),
		('local-package',?1,'Local','vpk','local_scan',?2,'left4dead2/addons/manual.vpk',1,'normal',1);
		INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint) VALUES
		('managed-package',?1,'left4dead2/addons/sourcemod/plugins/Managed.smx',?2,10,1),
		('managed-package',?1,'left4dead2/addons/sourcemod/translations/managed.phrases.txt',?2,20,0),
		('local-package',?1,'left4dead2/addons/manual.vpk',?2,30,1)`, instance.ID, sha); err != nil {
		t.Fatal(err)
	}
	paths, err := store.BackupManagedPackagePaths(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paths["left4dead2/addons/sourcemod/plugins/Managed.smx"] != "managed-package" ||
		paths["left4dead2/addons/sourcemod/translations/managed.phrases.txt"] != "managed-package" || len(paths) != 2 {
		t.Fatalf("managed backup paths = %#v", paths)
	}

	createdAt := time.Now().UTC().Truncate(time.Second)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO backups(id,instance_id,name,kind,path,platform,size,sha256,created_at)
		VALUES('backup-id',?1,'manual-safe.zip','manual','backups/instance/manual-safe.zip','windows/amd64',7,?2,?3)`, instance.ID, sha, createdAt); err != nil {
		t.Fatal(err)
	}
	job := domain.Job{InstanceID: instance.ID, Type: "backup.delete", Status: domain.JobSucceeded,
		Payload: map[string]any{"backup_id": "backup-id", "name": "manual-safe.zip"},
		Result:  map[string]any{"backup_id": "backup-id", "name": "manual-safe.zip", "already_absent": false}}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBackup(ctx, instance.ID, "backup-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted backup record is still present: %v", err)
	}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatalf("backup deletion projection is not idempotent: %v", err)
	}
}
