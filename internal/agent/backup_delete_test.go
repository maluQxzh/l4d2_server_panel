package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestDeleteBackupVerifiesIdentityAndDeletesTrustedArchive(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	instanceID := "instance-delete"
	name := "manual-20260812T120000Z-job.zip"
	dir := filepath.Join(dataDir, "backups", safeLogName(instanceID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("trusted backup")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	executor := &Executor{config: &Config{DataDir: dataDir}}
	result, err := executor.deleteBackup(domain.Job{InstanceID: instanceID, Payload: map[string]any{
		"backup_id": "backup-id", "name": name, "expected_size": len(content), "expected_sha256": hex.EncodeToString(sum[:]),
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["backup_id"] != "backup-id" || result["already_absent"] != false {
		t.Fatalf("unexpected deletion result: %#v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("backup still exists after deletion: %v", err)
	}
	result, err = executor.deleteBackup(domain.Job{InstanceID: instanceID, Payload: map[string]any{
		"backup_id": "backup-id", "name": name, "expected_size": len(content), "expected_sha256": hex.EncodeToString(sum[:]),
	}}, func(int, string) {})
	if err != nil || result["already_absent"] != true {
		t.Fatalf("missing backup deletion was not idempotent: %#v, %v", result, err)
	}
}

func TestDeleteBackupRejectsUnsafeOrChangedTarget(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	instanceID := "instance-delete"
	dir := filepath.Join(dataDir, "backups", safeLogName(instanceID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	name := "daily-safe.zip"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{config: &Config{DataDir: dataDir}}
	for _, payload := range []map[string]any{
		{"backup_id": "backup-id", "name": "../" + name},
		{"backup_id": "backup-id", "name": name, "expected_size": 7, "expected_sha256": strings.Repeat("0", 64)},
	} {
		if _, err := executor.deleteBackup(domain.Job{InstanceID: instanceID, Payload: payload}, func(int, string) {}); err == nil {
			t.Fatalf("unsafe deletion payload was accepted: %#v", payload)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rejected deletion changed the target: %v", err)
	}
}

func TestValidBackupArchiveNameAllowsOnlyPluginSevenZip(t *testing.T) {
	t.Parallel()
	if !validBackupArchiveName("plugin-20260812T120000Z-job.7z") {
		t.Fatal("expected plugin 7z archive name to be accepted")
	}
	for _, name := range []string{"plugin-job.zip", "manual-job.7z", "plugin-../job.7z"} {
		if validBackupArchiveName(name) {
			t.Fatalf("unexpected archive name accepted: %s", name)
		}
	}
}
