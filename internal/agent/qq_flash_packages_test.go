package agent

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestDiscoverQQPackageVPKsExtractsArchiveAndRejectsOtherFiles(t *testing.T) {
	staging := t.TempDir()
	archivePath := filepath.Join(staging, "mods.zip")
	writeQQPackageTestZIP(t, archivePath, map[string][]byte{
		"nested/campaign.vpk": validTestVPK([]byte("campaign"), 2),
	})
	state := &mapArchiveState{Root: staging, MaxEntries: 20, MaxEntrySize: maxPackageDownload, MaxExpanded: 8 << 20, DiskFreeReserve: 1, Subject: "test package"}
	inputs, err := discoverQQPackageVPKs(context.Background(), staging, archivePath, "mods.zip", 0, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].SortKey != "mods.zip!/nested/campaign.vpk" {
		t.Fatalf("unexpected discovered VPKs: %#v", inputs)
	}
	if _, _, err := validateVPK(inputs[0].Path); err != nil {
		t.Fatalf("discovered VPK was not valid: %v", err)
	}

	rejected := filepath.Join(staging, "mixed.zip")
	writeQQPackageTestZIP(t, rejected, map[string][]byte{
		"campaign.vpk": validTestVPK([]byte("campaign"), 1),
		"readme.txt":   []byte("not a package"),
	})
	state = &mapArchiveState{Root: staging, MaxEntries: 20, MaxEntrySize: maxPackageDownload, MaxExpanded: 8 << 20, DiskFreeReserve: 1, Subject: "test package"}
	if _, err := discoverQQPackageVPKs(context.Background(), staging, rejected, "mixed.zip", 0, state); err == nil || !strings.Contains(err.Error(), "only VPK") {
		t.Fatalf("archive containing a non-VPK file was accepted: %v", err)
	}
}

func TestDeletePackageRequiresRecordedHash(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	path := filepath.Join(instancePath, "left4dead2", "addons", "player-mod.vpk")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, validTestVPK([]byte("player mod"), 2), 0o640); err != nil {
		t.Fatal(err)
	}
	sha, _, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	job := testPackageDeleteJob(sha)
	job.Payload["expected_sha256"] = strings.Repeat("0", 64)
	if _, err := executor.deletePackage(context.Background(), job, func(int, string) {}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("package with a mismatched recorded hash was deleted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hash mismatch removed package: %v", err)
	}
	job = testPackageDeleteJob(sha)
	result, err := executor.deletePackage(context.Background(), job, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["path"] != "left4dead2/addons/player-mod.vpk" || result["sha256"] != sha {
		t.Fatalf("unexpected deletion result: %#v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("package remains after deletion: %v", err)
	}
}

func testPackageDeleteJob(sha string) domain.Job {
	return domain.Job{ID: "delete-package", InstanceID: "instance", Payload: map[string]any{
		"kind": "vpk", "package_path": "left4dead2/addons/player-mod.vpk", "expected_sha256": sha,
	}}
}

func writeQQPackageTestZIP(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	destination, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(destination)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
}
