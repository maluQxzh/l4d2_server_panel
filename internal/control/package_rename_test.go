package control

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func packageRenameStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "rename.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
	INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','/game',27015,'{}');
	INSERT INTO addon_packages(id,instance_id,name,kind,path,sha256) VALUES('package','instance','old','vpk','left4dead2/addons/old.vpk',?1)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPackageRenameQueueProjectionAndReplay(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	source, target := "left4dead2/addons/old.vpk", "left4dead2/addons/new.vpk"
	key := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
	storage := "package-versions/" + key("instance") + "/" + key(source) + "/" + strings.Repeat("b", 64) + ".bin"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO addon_package_versions(id,package_id,instance_id,source_job_id,sha256,storage_path,target_path,manifest) VALUES('version','package','instance','install',?1,?2,?3,json_object('files',json_array(json_object('path',?3))));
	INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size) VALUES('package','instance',?3,?1,5)`, strings.Repeat("c", 64), storage, source); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"package_id": "package", "name": "新的显示名", "filename": "new.vpk", "path": "/untrusted"}
	job, reused, err := s.QueueInstanceJob(ctx, "instance", "rename_package", payload, "", "rename-key")
	if err != nil || reused || job.Payload["path"] != source || job.Payload["target_path"] != target {
		t.Fatalf("queue: %#v %v %v", job, reused, err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "rename_package", payload, "", "another-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("write barrier missing: %v", err)
	}
	replay, reused, err := s.QueueInstanceJob(ctx, "instance", "rename_package", payload, "", "rename-key")
	if err != nil || !reused || replay.ID != job.ID {
		t.Fatalf("pending replay: %#v %v %v", replay, reused, err)
	}
	job.Status = domain.JobSucceeded
	job.Result = map[string]any{"path": target, "name": "新的显示名", "sha256": strings.Repeat("a", 64)}
	if err := s.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyJobResult(ctx, job); err != nil {
		t.Fatalf("projection replay: %v", err)
	}
	pkg, gotPath, err := s.PackageForToggle(ctx, "instance", "package")
	if err != nil || gotPath != target || pkg.Name != "新的显示名" {
		t.Fatalf("projection: %#v %s %v", pkg, gotPath, err)
	}
	versions, err := s.ListPackageVersions(ctx, "instance", "package")
	if err != nil || len(versions) != 1 || versions[0].TargetPath != target {
		t.Fatalf("versions: %#v %v", versions, err)
	}
	expected, err := domain.RenamedPackageSnapshot("instance", source, target, storage)
	if err != nil || versions[0].StoragePath != expected {
		t.Fatalf("storage path: %#v %v", versions, err)
	}
	var filePath, manifestPath string
	if err := s.db.QueryRowContext(ctx, `SELECT path FROM addon_package_files WHERE package_id='package'`).Scan(&filePath); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(manifest,'$.files[0].path') FROM addon_package_versions WHERE id='version'`).Scan(&manifestPath); err != nil {
		t.Fatal(err)
	}
	if filePath != target || manifestPath != target {
		t.Fatalf("inventories stale: %s %s", filePath, manifestPath)
	}
	job.Result["path"] = "left4dead2/addons/forged.vpk"
	if err := s.ApplyJobResult(ctx, job); err == nil {
		t.Fatal("forged result accepted")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded' WHERE id=?1`, job.ID); err != nil {
		t.Fatal(err)
	}
	replay, reused, err = s.QueueInstanceJob(ctx, "instance", "rename_package", payload, "", "rename-key")
	if err != nil || !reused || replay.ID != job.ID {
		t.Fatalf("completed replay: %v %v", reused, err)
	}
	payload["name"] = "different"
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "rename_package", payload, "", "rename-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed request reused: %v", err)
	}
}

func TestPackageRenameRejectsInvalidStateAndOwnership(t *testing.T) {
	for _, scenario := range []string{"running", "other_package", "other_disabled", "smart_owned", "invalid_name", "wrong_instance", "component_filename"} {
		t.Run(scenario, func(t *testing.T) {
			s := packageRenameStore(t)
			ctx := context.Background()
			payload := map[string]any{"package_id": "package", "name": "new", "filename": "new.vpk"}
			var query string
			switch scenario {
			case "running":
				query = `UPDATE instances SET status='running'`
			case "other_package":
				query = `INSERT INTO addon_packages(id,instance_id,name,kind,path) VALUES('other','instance','other','vpk','left4dead2/addons/new.vpk')`
			case "other_disabled":
				query = `INSERT INTO addon_packages(id,instance_id,name,kind,path) VALUES('other','instance','other','vpk','left4dead2/addons/panel-disabled/new.vpk')`
			case "smart_owned":
				query = `INSERT INTO addon_packages(id,instance_id,name,kind,path) VALUES('other','instance','other','smart',''); INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size) VALUES('other','instance','left4dead2/addons/new.vpk','` + strings.Repeat("a", 64) + `',1)`
			case "invalid_name":
				payload["filename"] = "../bad.vpk"
			case "wrong_instance":
				payload["package_id"] = "unowned"
			case "component_filename":
				query = `UPDATE addon_packages SET kind='sourcemod'`
			}
			if query != "" {
				if _, err := s.db.ExecContext(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.QueueInstanceJob(ctx, "instance", "rename_package", payload, "", ""); err == nil {
				t.Fatal("invalid rename accepted")
			}
		})
	}
}

func TestPackageRenamePermissionAndStatusScope(t *testing.T) {
	s := packageRenameStore(t)
	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, s, nil)
	defer server.Close()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"new"}`))
	route := chi.NewRouteContext()
	route.URLParams.Add("instanceID", "instance")
	route.URLParams.Add("packageID", "package")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{User: domain.User{ID: "observer", Role: domain.RoleViewer}}))
	response := httptest.NewRecorder()
	server.requireInstancePermission(PermissionPackagesInstall)(http.HandlerFunc(server.handleRenamePackage)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthorized rename: %d %s", response.Code, response.Body.String())
	}
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "rename_package", map[string]any{"package_id": "package", "name": "new"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	route.URLParams.Add("jobID", job.ID)
	response = httptest.NewRecorder()
	server.handlePackageOperationStatus(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "payload") || !strings.Contains(response.Body.String(), `"busy":true`) {
		t.Fatalf("status: %d %s", response.Code, response.Body.String())
	}
	route.URLParams.Keys[0] = "ignored"
	route.URLParams.Add("instanceID", "another")
	response = httptest.NewRecorder()
	server.handlePackageOperationStatus(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-instance status: %d", response.Code)
	}
}

func TestPackageRenameConcurrentScanDoesNotDuplicateOrResurrectPaths(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	rename, _, err := s.QueueInstanceJob(ctx, "instance", "rename_package", map[string]any{"package_id": "package", "name": "new", "filename": "new.vpk"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scan := domain.Job{ID: "scan", InstanceID: "instance", CreatedAt: rename.CreatedAt.Add(-time.Second), Result: map[string]any{
		"kind": "installed_vpk_scan", "directory": "left4dead2/addons", "valid_count": 2, "rejected_count": 0, "discovered_count": 2, "rejected": []any{},
		"packages": []any{
			map[string]any{"name": "old.vpk", "path": "left4dead2/addons/old.vpk", "sha256": strings.Repeat("a", 64), "size": 10, "vpk_version": 1},
			map[string]any{"name": "new.vpk", "path": "left4dead2/addons/new.vpk", "sha256": strings.Repeat("a", 64), "size": 10, "vpk_version": 1},
		},
	}}
	if err := s.applyInstalledPackageScanResult(ctx, scan); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListPackages(ctx, "instance")
	if err != nil || len(items) != 1 {
		t.Fatalf("scan duplicated pending rename: %#v %v", items, err)
	}
	rename.Result = map[string]any{"path": "left4dead2/addons/new.vpk", "name": "new", "sha256": strings.Repeat("a", 64)}
	if err := s.applyPackageRenameResult(ctx, rename); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded',finished_at=?2 WHERE id=?1`, rename.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.applyInstalledPackageScanResult(ctx, scan); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListPackages(ctx, "instance")
	if err != nil || len(items) != 1 || items[0].Path != "left4dead2/addons/new.vpk" {
		t.Fatalf("delayed scan resurrected old path: %#v %v", items, err)
	}
}
