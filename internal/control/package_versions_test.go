package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestParsePackageVersionMetadataRejectsUntrustedPaths(t *testing.T) {
	sha := strings.Repeat("a", 64)
	key := strings.Repeat("b", 64)
	job := strings.Repeat("c", 64)
	target := "left4dead2/addons/campaign.vpk"
	valid := map[string]any{
		"storage_path": "package-versions/" + key + "/" + key + "/" + job + ".bin",
		"target_path":  target, "sha256": sha, "size": float64(42),
	}
	metadata, ok := parsePackageVersionMetadata(valid, target)
	if !ok || metadata.Size != 42 || metadata.SHA256 != sha {
		t.Fatalf("valid package metadata was rejected: %#v %v", metadata, ok)
	}
	for _, path := range []string{"../../outside.bin", "package-versions/../outside.bin", "package-versions/not-a-key/file.bin"} {
		candidate := make(map[string]any, len(valid))
		for name, value := range valid {
			candidate[name] = value
		}
		candidate["storage_path"] = path
		if _, accepted := parsePackageVersionMetadata(candidate, target); accepted {
			t.Fatalf("unsafe storage path %q was accepted", path)
		}
	}
}

func TestPackageResultPathAllowlist(t *testing.T) {
	for _, candidate := range []struct{ kind, path string }{
		{"vpk", "left4dead2/addons/campaign.vpk"},
		{"plugin", "left4dead2/addons/sourcemod/plugins/welcome.smx"},
	} {
		if err := validateVersionedPackageResultPath(candidate.kind, candidate.path); err != nil {
			t.Errorf("safe target was rejected: %v", err)
		}
	}
	for _, candidate := range []struct{ kind, path string }{
		{"vpk", "left4dead2/cfg/campaign.vpk"},
		{"plugin", "left4dead2/addons/sourcemod/extensions/welcome.smx"},
		{"native", "left4dead2/addons/sourcemod/extensions/unsafe.dll"},
	} {
		if err := validateVersionedPackageResultPath(candidate.kind, candidate.path); err == nil {
			t.Errorf("unsafe/unsupported target was accepted: %#v", candidate)
		}
	}
}

func TestDeletablePackagePathAllowlist(t *testing.T) {
	for _, candidate := range []struct{ kind, path string }{
		{"vpk", "left4dead2/addons/player.vpk"},
		{"vpk", "left4dead2/addons/panel-disabled/player.vpk"},
		{"plugin", "left4dead2/addons/sourcemod/plugins/welcome.smx"},
		{"plugin", "left4dead2/addons/sourcemod/plugins/disabled/welcome.smx"},
	} {
		if err := validateDeletablePackagePath(candidate.kind, candidate.path); err != nil {
			t.Errorf("safe deletion target was rejected: %#v: %v", candidate, err)
		}
	}
	for _, candidate := range []struct{ kind, path string }{
		{"vpk", "left4dead2/addons/l4d2panel-map-id-1.vpk"},
		{"vpk", "left4dead2/cfg/player.vpk"},
		{"plugin", "left4dead2/addons/sourcemod/extensions/native.so"},
		{"native", "left4dead2/addons/sourcemod/extensions/native.so"},
	} {
		if err := validateDeletablePackagePath(candidate.kind, candidate.path); err == nil {
			t.Errorf("unsafe deletion target was accepted: %#v", candidate)
		}
	}
}

func TestGenericPackageRollbackEndpointIsRejected(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/actions",
		strings.NewReader(`{"type":"rollback_package","payload":{"snapshot_path":"injected"}}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
		User: domain.User{ID: "owner", Role: domain.RoleOwner},
	}))
	response := httptest.NewRecorder()
	server.handleInstanceAction(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "version-scoped") {
		t.Fatalf("generic rollback returned %d: %s", response.Code, response.Body.String())
	}
}

func TestInstalledPackageScanEndpointRejectsCallerPath(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/packages/scan", strings.NewReader(`{"path":"../../outside"}`))
	response := httptest.NewRecorder()
	server.handleScanInstalledPackages(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_package_scan") {
		t.Fatalf("scan with caller path returned %d: %s", response.Code, response.Body.String())
	}
}

func TestQQFlashPackageProjectionCreatesManagedPackagesAndDeleteRemovesOne(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "qq-packages.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','C:\\server',27015,'{}')`); err != nil {
		t.Fatal(err)
	}
	firstSHA, secondSHA := strings.Repeat("a", 64), strings.Repeat("b", 64)
	job := domain.Job{ID: "qq-job", InstanceID: "instance", Type: "qq_flash_import", Status: domain.JobSucceeded, Payload: map[string]any{"mode": "managed_vpk_v2"}, Result: map[string]any{
		"kind": "qq_flash_vpk",
		"packages": []any{
			map[string]any{"name": "one.vpk", "path": "left4dead2/addons/qq_job_0001_one.vpk", "sha256": firstSHA, "vpk_version": float64(2), "source_path": "one.vpk"},
			map[string]any{"name": "two.vpk", "path": "left4dead2/addons/qq_job_0002_two.vpk", "sha256": secondSHA, "vpk_version": float64(1), "source_path": "mods.zip!/two.vpk"},
		},
	}}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListPackages(ctx, "instance")
	if err != nil || len(items) != 2 {
		t.Fatalf("projected packages = %#v, err %v", items, err)
	}
	var first domain.AddonPackage
	for _, item := range items {
		if item.Name == "one.vpk" {
			first = item
		}
	}
	if first.ID == "" || first.Kind != "vpk" || !first.Enabled || first.Source != "qq_flash" || first.SHA256 != firstSHA {
		t.Fatalf("unexpected projected package: %#v", first)
	}
	deleteJob := domain.Job{ID: "delete-job", InstanceID: "instance", Type: "delete_package", Status: domain.JobSucceeded,
		Payload: map[string]any{"package_id": first.ID, "package_path": first.Path, "expected_sha256": first.SHA256},
		Result:  map[string]any{"path": first.Path, "sha256": first.SHA256}}
	if err := store.ApplyJobResult(ctx, deleteJob); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.ListPackages(ctx, "instance")
	if err != nil || len(remaining) != 1 || remaining[0].Name != "two.vpk" {
		t.Fatalf("remaining packages = %#v, err %v", remaining, err)
	}
}

func TestQQFlashPackageProjectionRejectsNonVPKPath(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "invalid-qq-package.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "qq-job", InstanceID: "instance", Result: map[string]any{
		"kind": "qq_flash_vpk", "packages": []any{map[string]any{
			"name": "bad.txt", "path": "left4dead2/addons/bad.txt", "sha256": strings.Repeat("a", 64), "vpk_version": float64(2),
		}},
	}}
	if err := store.applyQQFlashPackageResult(ctx, job); err == nil {
		t.Fatal("non-VPK QQ flash projection was accepted")
	}
}

func TestInstalledPackageScanProjectionIsPathIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "scanned-packages.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','C:\server',27015,'{}');
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk)
		VALUES('existing','instance','existing.vpk','vpk','upload',?1,'left4dead2/addons/existing.vpk',1,'normal')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{
		"kind": "installed_vpk_scan", "directory": "left4dead2/addons",
		"packages": []any{
			map[string]any{"name": "existing.vpk", "path": "left4dead2/addons/existing.vpk", "sha256": strings.Repeat("a", 64), "size": float64(100), "vpk_version": float64(1)},
			map[string]any{"name": "local.vpk", "path": "left4dead2/addons/local.vpk", "sha256": strings.Repeat("b", 64), "size": float64(200), "vpk_version": float64(2)},
		},
		"rejected": []any{}, "discovered_count": float64(2), "valid_count": float64(2), "rejected_count": float64(0),
	}
	job := domain.Job{ID: "scan-job", InstanceID: "instance", Type: "scan_packages", Status: domain.JobSucceeded, RequestedBy: "", Result: result}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListPackages(ctx, "instance")
	if err != nil || len(items) != 2 {
		t.Fatalf("projected packages = %#v, err %v", items, err)
	}
	var scanned domain.AddonPackage
	for _, item := range items {
		if item.Name == "local.vpk" {
			scanned = item
		}
	}
	if scanned.ID == "" || scanned.Source != "local_scan" || scanned.Version != "v2" || scanned.SHA256 != strings.Repeat("b", 64) || !scanned.Enabled {
		t.Fatalf("unexpected scanned package: %#v", scanned)
	}
}

func TestInstalledPackageScanProjectionRejectsReservedMapName(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "reserved-scanned-package.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','C:\server',27015,'{}')`); err != nil {
		t.Fatal(err)
	}
	job := domain.Job{InstanceID: "instance", Result: map[string]any{
		"kind": "installed_vpk_scan", "directory": "left4dead2/addons",
		"packages": []any{map[string]any{"name": "l4d2panel-map-one-1.vpk", "path": "left4dead2/addons/l4d2panel-map-one-1.vpk", "sha256": strings.Repeat("a", 64), "size": float64(100), "vpk_version": float64(1)}},
		"rejected": []any{}, "discovered_count": float64(1), "valid_count": float64(1), "rejected_count": float64(0),
	}}
	if err := store.applyInstalledPackageScanResult(ctx, job); err == nil {
		t.Fatal("reserved map-library VPK was accepted by scan projection")
	}
}

func TestSmartPluginDetailsAndFileEditProjection(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "smart-file-edit.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	packageID := "11111111-1111-4111-8111-111111111111"
	oldSHA, newSHA, aggregate := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','C:\server',27015,'{}');
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,file_count)
		VALUES(?1,'instance','Sample','smart','sample.zip',?2,'',1,'normal',2);
		INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint) VALUES
		(?1,'instance','left4dead2/cfg/sourcemod/sample.cfg',?2,10,0),
		(?1,'instance','left4dead2/addons/sourcemod/plugins/sample.smx',?2,10,1)`, packageID, oldSHA); err != nil {
		t.Fatal(err)
	}
	pkg, files, err := store.PackageDetails(ctx, "instance", packageID)
	if err != nil || pkg.Name != "Sample" || len(files) != 2 || !files[1].Editable && !files[0].Editable {
		t.Fatalf("unexpected package details: pkg=%#v files=%#v err=%v", pkg, files, err)
	}
	for _, file := range files {
		if file.Entrypoint && file.Editable {
			t.Fatal("smart plugin entrypoint was exposed as editable")
		}
	}
	job := domain.Job{ID: "edit-job", InstanceID: "instance", Type: "file", Status: domain.JobSucceeded,
		Payload: map[string]any{"operation": "smart_plugin_edit", "package_id": packageID, "relative_path": "left4dead2/cfg/sourcemod/sample.cfg", "expected_sha256": oldSHA},
		Result: map[string]any{"package_id": packageID, "path": "left4dead2/cfg/sourcemod/sample.cfg", "previous_sha256": oldSHA,
			"sha256": newSHA, "size": float64(15), "aggregate_sha256": aggregate}}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatalf("replayed projection failed: %v", err)
	}
	pkg, files, err = store.PackageDetails(ctx, "instance", packageID)
	if err != nil || pkg.SHA256 != aggregate {
		t.Fatalf("package aggregate was not projected: pkg=%#v err=%v", pkg, err)
	}
	for _, file := range files {
		if file.Path == "left4dead2/cfg/sourcemod/sample.cfg" && (file.SHA256 != newSHA || file.Size != 15) {
			t.Fatalf("file edit was not projected: %#v", file)
		}
	}
}

func TestServerExtensionProjectionKeepsOneManagedComponentRecord(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "server-extension.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','C:\server',27015,'{}');
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk)
		VALUES('11111111-1111-4111-8111-111111111111','instance','Old SourceMod','sourcemod','upload',?1,'',1,'normal'),
		('22222222-2222-4222-8222-222222222222','instance','Duplicate SourceMod','sourcemod','upload',?1,'',1,'normal')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	packageID := "11111111-1111-4111-8111-111111111111"
	first := domain.Job{ID: "33333333-3333-4333-8333-333333333333", InstanceID: "instance", Type: "install_mod", Status: domain.JobSucceeded,
		Payload: map[string]any{"kind": "sourcemod", "package_id": packageID, "source": "sourcemod.zip"},
		Result: map[string]any{"kind": "sourcemod", "package_id": packageID, "version": "1.12", "source": "sourcemod.zip", "sha256": strings.Repeat("b", 64),
			"managed_files": []any{
				map[string]any{"path": "left4dead2/addons/sourcemod/bin/sourcemod_mm.dll", "sha256": strings.Repeat("c", 64), "size": float64(100)},
				map[string]any{"path": "left4dead2/addons/sourcemod/plugins/basecommands.smx", "sha256": strings.Repeat("d", 64), "size": float64(200)},
			}},
	}
	if err := store.ApplyJobResult(ctx, first); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListPackages(ctx, "instance")
	if err != nil || len(items) != 1 || items[0].ID != packageID || items[0].Name != "SourceMod" || items[0].FileCount != 2 {
		t.Fatalf("unexpected projected SourceMod component: %#v, err %v", items, err)
	}
	_, files, err := store.PackageDetails(ctx, "instance", packageID)
	if err != nil || len(files) != 2 {
		t.Fatalf("unexpected SourceMod inventory: %#v, err %v", files, err)
	}

	second := first
	second.ID = "44444444-4444-4444-8444-444444444444"
	second.Result = map[string]any{"kind": "sourcemod", "package_id": packageID, "version": "1.13", "source": "sourcemod-new.zip", "sha256": strings.Repeat("e", 64),
		"managed_files": []any{map[string]any{
			"path": "left4dead2/addons/sourcemod/bin/sourcemod_mm.dll", "sha256": strings.Repeat("f", 64), "size": float64(120),
		}},
	}
	if err := store.ApplyJobResult(ctx, second); err != nil {
		t.Fatal(err)
	}
	items, err = store.ListPackages(ctx, "instance")
	if err != nil || len(items) != 1 || items[0].ID != packageID || items[0].Version != "1.13" || items[0].FileCount != 1 {
		t.Fatalf("SourceMod update was not canonicalized: %#v, err %v", items, err)
	}
	_, files, err = store.PackageDetails(ctx, "instance", packageID)
	if err != nil || len(files) != 1 || files[0].SHA256 != strings.Repeat("f", 64) {
		t.Fatalf("SourceMod inventory was not replaced: %#v, err %v", files, err)
	}
}

func TestServerExtensionProjectionRejectsProtectedAdminsFile(t *testing.T) {
	for _, candidate := range []struct{ kind, path string }{
		{"metamod", "left4dead2/addons/metamod_x64.vdf"},
		{"sourcemod", "left4dead2/addons/metamod/sourcemod.vdf"},
		{"sourcemod", "left4dead2/cfg/sourcemod/sourcemod.cfg"},
	} {
		if err := validateServerExtensionManagedPath(candidate.kind, candidate.path); err != nil {
			t.Fatalf("official component path was rejected: %#v: %v", candidate, err)
		}
	}
	job := domain.Job{ID: "11111111-1111-4111-8111-111111111111", InstanceID: "instance", Result: map[string]any{
		"kind": "sourcemod", "sha256": strings.Repeat("a", 64),
		"managed_files": []any{map[string]any{
			"path": "left4dead2/addons/sourcemod/configs/admins_simple.ini", "sha256": strings.Repeat("b", 64), "size": float64(1),
		}},
	}}
	store := &Store{}
	if err := store.applyServerExtensionInstallResult(context.Background(), job, "sourcemod"); err == nil {
		t.Fatal("protected admins_simple.ini was accepted into the managed deletion inventory")
	}
}

func TestServerExtensionDeleteRequiresInventoryAndPreservesDependencyOrder(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "server-extension-delete.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, _, err := store.BootstrapOwner(ctx, "owner", "Owner", "password-hash")
	if err != nil {
		t.Fatal(err)
	}
	const instanceID = "11111111-1111-4111-8111-111111111111"
	const sourceModID = "22222222-2222-4222-8222-222222222222"
	const metamodID = "33333333-3333-4333-8333-333333333333"
	sha := strings.Repeat("a", 64)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES(?1,'node','Server','stopped','C:\server',27015,'{}');
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,file_count) VALUES
		(?2,?1,'SourceMod','sourcemod','upload',?4,'',false,'normal',0),
		(?3,?1,'Metamod:Source','metamod','upload',?4,'',true,'normal',1);
		INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint)
		VALUES(?3,?1,'left4dead2/addons/metamod.vdf',?4,10,true)`, instanceID, sourceModID, metamodID, sha); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	requestDelete := func(packageID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/instances/"+instanceID+"/packages/"+packageID, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", instanceID)
		route.URLParams.Add("packageID", packageID)
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{User: owner}))
		response := httptest.NewRecorder()
		server.handleDeletePackage(response, request)
		return response
	}

	response := requestDelete(sourceModID)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "component_inventory_required") {
		t.Fatalf("legacy SourceMod delete returned %d: %s", response.Code, response.Body.String())
	}
	response = requestDelete(metamodID)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "sourcemod_depends_on_metamod") {
		t.Fatalf("Metamod dependency delete returned %d: %s", response.Code, response.Body.String())
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM addon_packages WHERE id=?1;
		INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,file_count)
		VALUES(?1,?2,'SourceMod','sourcemod','upload',?3,'',true,'normal',1);
		INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint)
		VALUES(?1,?2,'left4dead2/addons/sourcemod/bin/sourcemod_mm.dll',?3,20,true)`, sourceModID, instanceID, sha); err != nil {
		t.Fatal(err)
	}
	response = requestDelete(sourceModID)
	if response.Code != http.StatusAccepted {
		t.Fatalf("managed SourceMod delete returned %d: %s", response.Code, response.Body.String())
	}
	var payload string
	if err := store.db.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE instance_id=?1 AND type='delete_package' ORDER BY created_at DESC LIMIT 1`, instanceID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, "left4dead2/addons/sourcemod/bin/sourcemod_mm.dll") || strings.Contains(payload, "admins_simple.ini") {
		t.Fatalf("unexpected SourceMod deletion payload: %s", payload)
	}
}
