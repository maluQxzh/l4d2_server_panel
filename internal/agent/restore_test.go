package agent

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCrossPlatformRestoreSkipsNativeBinaries(t *testing.T) {
	base := t.TempDir()
	instance := filepath.Join(base, "instance")
	if err := os.MkdirAll(instance, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "backup.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	sourceNativeName, targetNativeName := "engine.so", "engine.dll"
	if runtime.GOOS == "linux" {
		sourceNativeName, targetNativeName = "engine.dll", "engine.so"
	}
	sourceNativeRelative := "left4dead2/addons/sourcemod/bin/" + sourceNativeName
	files := map[string][]byte{
		"left4dead2/cfg/server.cfg":                      []byte("hostname test\n"),
		"left4dead2/addons/sourcemod/plugins/test.smx":   []byte("portable"),
		"left4dead2/addons/sourcemod/configs/admins.cfg": []byte("source-user-config"),
		sourceNativeRelative:                             []byte("source-native"),
	}
	manifest := BackupManifest{Version: backupLegacyFormatVersion, InstanceID: "server-1", OS: "linux", Arch: "amd64", CreatedAt: time.Now(), Files: make(map[string]string)}
	if runtime.GOOS == "linux" {
		manifest.OS = "windows"
	}
	for name, content := range files {
		entry, err := writer.Create("data/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		manifest.Files[name] = hex.EncodeToString(sum[:])
	}
	manifestEntry, err := writer.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(manifest)
	if _, err := manifestEntry.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	targetConfig := filepath.Join(instance, "left4dead2", "addons", "sourcemod", "configs", "admins.cfg")
	if err := os.MkdirAll(filepath.Dir(targetConfig), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetConfig, []byte("official-default"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetNative := filepath.Join(instance, "left4dead2", "addons", "sourcemod", "bin", targetNativeName)
	if err := os.MkdirAll(filepath.Dir(targetNative), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetNative, []byte("target-native"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleWrongPlatformNative := filepath.Join(instance, filepath.FromSlash(sourceNativeRelative))
	if err := os.WriteFile(staleWrongPlatformNative, []byte("stale-wrong-platform"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := RestoreBackup(instance, archive, filepath.Join(base, "restore-test"))
	if err != nil {
		t.Fatal(err)
	}
	if got.OS == runtime.GOOS || len(skipped) != 1 || skipped[0] != sourceNativeRelative {
		t.Fatalf("manifest=%#v skipped=%#v", got, skipped)
	}
	if _, err := os.Stat(filepath.Join(instance, "left4dead2", "cfg", "server.cfg")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(instance, "left4dead2", "addons", "sourcemod", "plugins", "test.smx")); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(targetNative); err != nil || string(raw) != "target-native" {
		t.Fatalf("target native binary was not preserved: %q, %v", raw, err)
	}
	if _, err := os.Stat(staleWrongPlatformNative); !os.IsNotExist(err) {
		t.Fatalf("wrong-platform native binary survived restore: %v", err)
	}
	if raw, err := os.ReadFile(targetConfig); err != nil || string(raw) != "source-user-config" {
		t.Fatalf("source user config did not win the final portable overlay: %q, %v", raw, err)
	}
}

func TestNativeFileSelectionMatchesTargetOS(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path, goos string
		want       bool
	}{
		{"addons/ext.dll", "windows", true},
		{"addons/ext.so", "windows", false},
		{"addons/ext.so", "linux", true},
		{"addons/ext.dll", "linux", false},
		{"addons/ext.dylib", "linux", false},
	} {
		if got := isNativeFileForOS(test.path, test.goos); got != test.want {
			t.Fatalf("isNativeFileForOS(%q, %q) = %v, want %v", test.path, test.goos, got, test.want)
		}
	}
}
