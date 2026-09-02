package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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

func TestPluginBackupInventoryAndProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "plugin-backup.sqlite3"))
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
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "plugin backup", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 64)
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,file_count) VALUES
		('smart-package',?1,'Smart Plugin','smart','upload',?2,'',1,'normal',2),
		('legacy-package',?1,'Legacy Plugin','smx','upload',?2,'left4dead2/addons/sourcemod/plugins/legacy.smx',0,'normal',1),
		('component-package',?1,'SourceMod','sourcemod','url',?2,'',1,'normal',1),
		('vpk-package',?1,'Campaign','vpk','upload',?2,'left4dead2/addons/campaign.vpk',1,'normal',1);
		INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint) VALUES
		('smart-package',?1,'left4dead2/addons/sourcemod/plugins/smart.smx',?2,10,1),
		('smart-package',?1,'left4dead2/cfg/sourcemod/smart.cfg',?2,20,0)`, instance.ID, sha); err != nil {
		t.Fatal(err)
	}
	packages, err := store.PluginBackupPackages(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || packages[0].ID != "legacy-package" || len(packages[0].Files) != 1 ||
		packages[0].Files[0].Path != "left4dead2/addons/sourcemod/plugins/legacy.smx" || packages[0].Enabled ||
		packages[1].ID != "smart-package" || len(packages[1].Files) != 2 {
		t.Fatalf("unexpected plugin backup inventory: %#v", packages)
	}
	createdAt := time.Now().UTC().Truncate(time.Second)
	job := domain.Job{ID: "plugin-backup-job", InstanceID: instance.ID, Type: "plugin.backup", Status: domain.JobSucceeded,
		Payload: map[string]any{"layout": "compact"}, Result: map[string]any{
			"name": "plugin-20260812T120000Z-job.7z", "path": `D:\agent\data\backups\instance\plugin-20260812T120000Z-job.7z`,
			"kind": "plugin_compact", "platform": "windows/amd64", "size": 123, "sha256": sha, "created_at": createdAt.Format(time.RFC3339Nano),
		}}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	backups, err := store.ListBackups(ctx, instance.ID)
	if err != nil || len(backups) != 1 || backups[0].Kind != "plugin_compact" || backups[0].Name != job.Result["name"] {
		t.Fatalf("unexpected plugin backup projection: %#v, %v", backups, err)
	}
	job.Result["kind"] = "plugin_preserve"
	if err := store.ApplyJobResult(ctx, job); err == nil {
		t.Fatal("mismatched plugin backup layout was projected")
	}
}

func TestAgentPathBaseAcceptsWindowsAndLinuxPaths(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		`D:\agent\data\backups\instance\plugin-job.7z`,
		"/var/lib/l4d2panel/backups/instance/plugin-job.7z",
		"backups/instance/plugin-job.7z",
	} {
		if actual := agentPathBase(value); actual != "plugin-job.7z" {
			t.Errorf("agentPathBase(%q) = %q", value, actual)
		}
	}
}

func TestCreatePluginBackupBuildsServerOwnedInventory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "plugin-backup-handler.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: %v, %v", created, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "plugin backup", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("c", 64)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,file_count)
		VALUES('plugin-package',?1,'Plugin','plugin','upload',?2,'left4dead2/addons/sourcemod/plugins/plugin.smx',1,'normal',1)`, instance.ID, sha); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/"+instance.ID+"/plugin-backups", strings.NewReader(`{"layout":"compact"}`))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("instanceID", instance.ID)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{User: owner}))
	response := httptest.NewRecorder()
	server.handleCreatePluginBackup(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create plugin backup returned %d: %s", response.Code, response.Body.String())
	}
	var jobType string
	var rawPayload []byte
	if err := store.db.QueryRowContext(ctx, `SELECT type,payload FROM jobs WHERE instance_id=?1 ORDER BY created_at DESC LIMIT 1`, instance.ID).Scan(&jobType, &rawPayload); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Layout   string                       `json:"layout"`
		Packages []domain.PluginBackupPackage `json:"plugin_packages"`
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatal(err)
	}
	if jobType != "plugin.backup" || payload.Layout != "compact" || len(payload.Packages) != 1 ||
		len(payload.Packages[0].Files) != 1 || payload.Packages[0].Files[0].Path != "left4dead2/addons/sourcemod/plugins/plugin.smx" {
		t.Fatalf("unexpected queued plugin backup: type=%q payload=%#v", jobType, payload)
	}
}
