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

func TestSelectiveBackupOnlyIncludesConfigurationAndManagedPackages(t *testing.T) {
	t.Parallel()
	instance := t.TempDir()
	files := map[string]string{
		"left4dead2/cfg/server.cfg":                                 "hostname selective\n",
		"left4dead2/cfg/l4d2panel.cfg":                              "rcon_password secret\n",
		"left4dead2/addons/sourcemod/configs/admins_simple.ini":     "STEAM_1:1:1\n",
		"left4dead2/addons/sourcemod/plugins/Panel-Upload.smx":      "managed plugin",
		"left4dead2/addons/panel-upload.vpk":                        "managed mod",
		"left4dead2/addons/manual.vpk":                              "manual mod",
		"left4dead2/addons/workshop/l4d2panel-map-example-00.vpk":   "assigned map",
		"left4dead2/addons/sourcemod/logs/errors_20260812.log":      "runtime log",
		"left4dead2/addons/sourcemod/data/sqlite-storage-local.sq3": "runtime data",
	}
	for relative, content := range files {
		path := filepath.Join(instance, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	managed := map[string]string{
		"left4dead2/addons/sourcemod/plugins/Panel-Upload.smx": "plugin-package",
		"left4dead2/addons/panel-upload.vpk":                   "mod-package",
	}
	archive := filepath.Join(t.TempDir(), "selective.zip")
	manifest, err := CreateSelectiveBackupWithMaps("instance", instance, archive,
		BackupLaunchConfig{Mode: "coop", StartMap: "c1m1_hotel", MaxPlayers: 8}, nil, managed)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != backupSelectiveVersion || manifest.Scope != backupSelectiveScope {
		t.Fatalf("selective manifest version/scope = %d/%q", manifest.Version, manifest.Scope)
	}
	for _, expected := range []string{
		"left4dead2/cfg/server.cfg",
		"left4dead2/addons/sourcemod/configs/admins_simple.ini",
		"left4dead2/addons/sourcemod/plugins/Panel-Upload.smx",
		"left4dead2/addons/panel-upload.vpk",
	} {
		if _, ok := manifest.Files[expected]; !ok {
			t.Errorf("expected selective file %q is missing", expected)
		}
	}
	for _, excluded := range []string{
		"left4dead2/cfg/l4d2panel.cfg",
		"left4dead2/addons/manual.vpk",
		"left4dead2/addons/workshop/l4d2panel-map-example-00.vpk",
		"left4dead2/addons/sourcemod/logs/errors_20260812.log",
		"left4dead2/addons/sourcemod/data/sqlite-storage-local.sq3",
	} {
		if _, ok := manifest.Files[excluded]; ok {
			t.Errorf("excluded file %q was backed up", excluded)
		}
	}
	if len(manifest.Managed) != 2 {
		t.Fatalf("managed manifest files = %#v", manifest.Managed)
	}

	target := t.TempDir()
	manual := filepath.Join(target, "left4dead2", "addons", "manual.vpk")
	staleConfig := filepath.Join(target, "left4dead2", "cfg", "stale.cfg")
	for path, content := range map[string]string{manual: "keep manual", staleConfig: "remove stale"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := RestoreBackup(target, archive, filepath.Join(t.TempDir(), "restore-selective")); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(manual); err != nil || string(raw) != "keep manual" {
		t.Fatalf("selective restore changed unmanaged addon: %q, %v", raw, err)
	}
	if _, err := os.Stat(staleConfig); !os.IsNotExist(err) {
		t.Fatalf("selective restore retained stale configuration: %v", err)
	}
}

func twoDigits(value int) string {
	if value < 10 {
		return "0" + string(rune('0'+value))
	}
	return "10"
}
