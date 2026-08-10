package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
