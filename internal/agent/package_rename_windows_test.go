package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestRenamePackageRestoresHistoryWhenWindowsFileIsLocked(t *testing.T) {
	e, data, root := uploadedPackageExecutor(t)
	e.processes = &ProcessManager{items: map[string]*managedProcess{}}
	source, target := "left4dead2/addons/old.vpk", "left4dead2/addons/new.vpk"
	sha := renameTestFile(t, root, source, "original")
	storage := "package-versions/" + packageVersionKey("instance") + "/" + packageVersionKey(source) + "/" + strings.Repeat("b", 64) + ".bin"
	historySHA := renameTestFile(t, data, storage, "previous")
	// Model a game/plugin handle that permits reads but denies deletion/rename.
	locked, err := openManagedFileNoFollow(root, source)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	job := domain.Job{InstanceID: "instance", Payload: map[string]any{"package_id": "package", "name": "new", "kind": "vpk", "path": source, "target_path": target, "expected_sha256": sha, "snapshots": []any{map[string]any{"storage_path": storage, "sha256": historySHA}}}}
	if _, err := e.renamePackage(context.Background(), job, func(int, string) {}); err == nil {
		t.Fatal("locked source was renamed")
	}
	actual, _, err := hashRegularFileWithinNoFollow(data, storage, maxPackageDownload)
	if err != nil || actual != historySHA {
		t.Fatalf("history was not rolled back: %s %v", actual, err)
	}
	renamed, err := domain.RenamedPackageSnapshot("instance", source, target, storage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, renamed)); !os.IsNotExist(err) {
		t.Fatalf("migrated history still exists: %v", err)
	}
	actual, _, err = hashRegularFileWithinNoFollow(root, source, maxPackageDownload)
	if err != nil || actual != sha {
		t.Fatalf("source changed: %s %v", actual, err)
	}
}
