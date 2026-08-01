package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func testVPK(marker byte) []byte {
	return validTestVPK([]byte{marker}, 2)
}

func installUploadedVersion(t *testing.T, executor *Executor, jobID, name, kind string, raw []byte) map[string]any {
	t.Helper()
	result, err := executor.installMod(context.Background(), domain.Job{ID: jobID, InstanceID: "instance", Payload: map[string]any{
		"kind": kind, "name": name, "content_base64": base64.StdEncoding.EncodeToString(raw),
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestVPKUpgradePreservesAndRollsBackVersion(t *testing.T) {
	executor, dataDir, instancePath := uploadedPackageExecutor(t)
	first := installUploadedVersion(t, executor, "vpk-a", "campaign.vpk", "vpk_url", testVPK('A'))
	if first["previous_version"] != nil {
		t.Fatalf("first install unexpectedly created history: %#v", first)
	}
	second := installUploadedVersion(t, executor, "vpk-b", "campaign.vpk", "vpk_url", testVPK('B'))
	previous, ok := second["previous_version"].(map[string]any)
	if !ok {
		t.Fatalf("upgrade did not return a recoverable version: %#v", second)
	}
	storagePath, _ := previous["storage_path"].(string)
	if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(storagePath))); err != nil {
		t.Fatalf("saved version is missing: %v", err)
	}

	result, err := executor.rollbackPackage(context.Background(), domain.Job{ID: "rollback-a", InstanceID: "instance", Payload: map[string]any{
		"kind": "vpk", "package_path": "left4dead2/addons/campaign.vpk", "snapshot_path": storagePath,
		"expected_sha256": previous["sha256"], "restore_version": "v2-A",
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["version"] != "v2-A" || result["previous_version"] == nil {
		t.Fatalf("unexpected rollback result: %#v", result)
	}
	raw, err := os.ReadFile(filepath.Join(instancePath, "left4dead2", "addons", "campaign.vpk"))
	if err != nil || len(raw) == 0 || raw[len(raw)-1] != 'A' {
		t.Fatalf("rollback did not restore version A: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(storagePath))); !os.IsNotExist(err) {
		t.Fatalf("consumed saved version remains accessible: %v", err)
	}
}

func TestPackageSnapshotRetentionKeepsNewestThree(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	targetRelative := "left4dead2/addons/campaign.vpk"
	target := filepath.Join(instancePath, filepath.FromSlash(targetRelative))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, testVPK('A'), 0o640); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 4)
	baseTime := time.Now().Add(-time.Hour)
	for index := 0; index < 4; index++ {
		snapshot, err := executor.createPackageSnapshot("instance", "retention-"+string(rune('a'+index)), targetRelative, target)
		if err != nil {
			t.Fatal(err)
		}
		absolute := filepath.Join(executor.config.DataDir, filepath.FromSlash(snapshot.StoragePath))
		when := baseTime.Add(time.Duration(index) * time.Minute)
		if err := os.Chtimes(absolute, when, when); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, snapshot.StoragePath)
	}
	removed, err := executor.prunePackageSnapshots("instance", targetRelative)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != paths[0] {
		t.Fatalf("unexpected retention result: removed=%v paths=%v", removed, paths)
	}
	for index, path := range paths {
		_, statErr := os.Stat(filepath.Join(executor.config.DataDir, filepath.FromSlash(path)))
		if index == 0 && !os.IsNotExist(statErr) {
			t.Fatalf("oldest snapshot was retained: %v", statErr)
		}
		if index > 0 && statErr != nil {
			t.Fatalf("new snapshot %d was removed: %v", index, statErr)
		}
	}
}

func TestSMXUpgradePreservesVersionAndRollbackRejectsUntrustedPaths(t *testing.T) {
	executor, _, _ := uploadedPackageExecutor(t)
	installUploadedVersion(t, executor, "smx-a", "welcome.smx", "plugin", []byte("plugin A"))
	second := installUploadedVersion(t, executor, "smx-b", "welcome.smx", "plugin", []byte("plugin B"))
	previous, ok := second["previous_version"].(map[string]any)
	if !ok {
		t.Fatalf("SMX upgrade did not preserve a version: %#v", second)
	}
	if _, err := executor.rollbackPackage(context.Background(), domain.Job{ID: "unsafe", InstanceID: "instance", Payload: map[string]any{
		"kind": "plugin", "package_path": "left4dead2/addons/sourcemod/plugins/welcome.smx",
		"snapshot_path": "../../outside.bin", "expected_sha256": previous["sha256"],
	}}, func(int, string) {}); err == nil {
		t.Fatal("rollback accepted a snapshot outside the Agent-owned version directory")
	}
	if _, err := executor.rollbackPackage(context.Background(), domain.Job{ID: "unsafe-target", InstanceID: "instance", Payload: map[string]any{
		"kind": "plugin", "package_path": "left4dead2/cfg/welcome.smx",
		"snapshot_path": previous["storage_path"], "expected_sha256": previous["sha256"],
	}}, func(int, string) {}); err == nil {
		t.Fatal("rollback accepted a target outside the approved SourceMod plugins directory")
	}
}

func TestFailedPackageUpgradeRestoresCurrentFileAndDropsFailedSnapshot(t *testing.T) {
	executor, dataDir, instancePath := uploadedPackageExecutor(t)
	installUploadedVersion(t, executor, "stable-a", "campaign.vpk", "vpk_url", testVPK('A'))
	executor.packageMutationVerifier = func(string, string) error { return errors.New("simulated post-copy verification failure") }
	_, err := executor.installMod(context.Background(), domain.Job{ID: "failed-b", InstanceID: "instance", Payload: map[string]any{
		"kind": "vpk_url", "name": "campaign.vpk", "content_base64": base64.StdEncoding.EncodeToString(testVPK('B')),
	}}, func(int, string) {})
	if err == nil {
		t.Fatal("expected the simulated verification failure")
	}
	raw, readErr := os.ReadFile(filepath.Join(instancePath, "left4dead2", "addons", "campaign.vpk"))
	if readErr != nil || len(raw) == 0 || raw[len(raw)-1] != 'A' {
		t.Fatalf("failed upgrade did not restore version A: %q %v", raw, readErr)
	}
	root := filepath.Join(dataDir, "package-versions")
	var saved int
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && filepath.Ext(entry.Name()) == ".bin" {
			saved++
		}
		return nil
	})
	if saved != 0 {
		t.Fatalf("failed upgrade left %d untracked snapshot files", saved)
	}
}
