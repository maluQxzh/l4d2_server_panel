package agent

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupExcludesGeneratedSecretConfig(t *testing.T) {
	t.Parallel()
	instance := t.TempDir()
	cfg := filepath.Join(instance, "left4dead2", "cfg")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "server.cfg"), []byte("hostname test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "l4d2panel.cfg"), []byte("rcon_password secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	manifest, err := CreateBackup("instance-1", instance, archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest.Files["left4dead2/cfg/l4d2panel.cfg"]; exists {
		t.Fatal("generated RCON configuration was included in backup manifest")
	}
	if _, exists := manifest.Files["left4dead2/cfg/server.cfg"]; !exists {
		t.Fatal("user server.cfg was not included in backup")
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if strings.EqualFold(entry.Name, "data/left4dead2/cfg/l4d2panel.cfg") {
			t.Fatal("generated RCON configuration was included in backup archive")
		}
	}
}

func TestBackupRetentionKeepsDailyAndWeeklyLimits(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var current string
	for i := 0; i < 10; i++ {
		name := "daily-202607" + twoDigits(i) + "T040000Z-job.zip"
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if i == 9 {
			current = path
		}
	}
	pruned, weekly, err := ApplyBackupRetention(dir, current, "daily", 7, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 3 {
		t.Fatalf("pruned = %d, want 3", len(pruned))
	}
	for _, path := range pruned {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("pruned path still exists: %s (%v)", path, err)
		}
	}
	if weekly == "" || !isWithin(dir, weekly) {
		t.Fatalf("weekly path = %q", weekly)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	daily, weeks := 0, 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "daily-") {
			daily++
		}
		if strings.HasPrefix(entry.Name(), "weekly-") {
			weeks++
		}
	}
	if daily != 7 || weeks != 1 {
		t.Fatalf("daily=%d weekly=%d", daily, weeks)
	}
}

func TestRestoreReconcilesPortableFilesAndPreservesGeneratedSecret(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	for _, root := range []string{source, target} {
		if err := os.MkdirAll(filepath.Join(root, "left4dead2", "cfg"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "left4dead2", "cfg", "server.cfg"), []byte("hostname restored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "left4dead2", "cfg", "stale.cfg"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(target, "left4dead2", "cfg", "l4d2panel.cfg")
	if err := os.WriteFile(secretPath, []byte("rcon_password current-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := CreateBackup("instance", source, archive); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RestoreBackup(target, archive, filepath.Join(t.TempDir(), "restore-test")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "left4dead2", "cfg", "stale.cfg")); !os.IsNotExist(err) {
		t.Fatalf("stale portable file survived restore: %v", err)
	}
	if raw, err := os.ReadFile(secretPath); err != nil || !strings.Contains(string(raw), "current-secret") {
		t.Fatalf("generated secret was not preserved: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(target, "left4dead2", "cfg", "server.cfg")); err != nil || string(raw) != "hostname restored\n" {
		t.Fatalf("portable config was not restored: %q, %v", raw, err)
	}
}

func twoDigits(value int) string {
	if value < 10 {
		return "0" + string(rune('0'+value))
	}
	return "10"
}
