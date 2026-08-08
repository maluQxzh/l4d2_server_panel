package agent

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestBackupV2CapturesPortableLaunchConfigWithoutSecrets(t *testing.T) {
	t.Parallel()
	instancePath := t.TempDir()
	args, err := BuildStartArgs(StartOptions{
		Port: 27017, TVPort: 27021, MaxPlayers: 12, Map: "c5m1_waterfront", Mode: "versus",
		Difficulty: "hard", ExtraArgs: []string{"-secure", "+sv_tags", "community"},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := domain.InstanceConfig{
		Hostname: "Source server", Mode: "versus", Difficulty: "hard", MaxPlayers: 12, StartMap: "c5m1_waterfront",
		Region: "2", SteamGroupID: "123456789", CVars: map[string]string{"sv_tags": "coop,custom"},
	}
	if err := writeGeneratedServerConfig(instancePath, []byte("rcon_password \"source-rcon-secret\"\nsv_password \"source-server-secret\"\n")); err != nil {
		t.Fatal(err)
	}
	record := InstanceRecord{Args: args, Config: config, RCONPort: 27017, TVPort: 27021, AutoRestart: true}
	launch, err := backupLaunchFromRecord(record, instancePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "portable-v2.zip")
	created, err := CreateBackupWithLaunch("source-instance", instancePath, archive, launch)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadBackupManifest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != backupLaunchFormatVersion || manifest.Launch == nil {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	if !reflect.DeepEqual(created.Launch, manifest.Launch) || manifest.Launch.Mode != "versus" || manifest.Launch.StartMap != "c5m1_waterfront" || manifest.Launch.MaxPlayers != 12 || !manifest.Launch.AutoRestart {
		t.Fatalf("portable launch config was not captured: %#v", manifest.Launch)
	}
	if !reflect.DeepEqual(manifest.Launch.ExtraArgs, []string{"-secure", "+sv_tags", "community"}) {
		t.Fatalf("extra args = %#v", manifest.Launch.ExtraArgs)
	}
	if manifest.Launch.Hostname != "Source server" || manifest.Launch.CVars["sv_tags"] != "coop,custom" {
		t.Fatalf("safe stored configuration was not captured: %#v", manifest.Launch)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var archiveContents bytes.Buffer
	for _, entry := range reader.File {
		file, openErr := entry.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, copyErr := io.Copy(&archiveContents, file); copyErr != nil {
			_ = file.Close()
			t.Fatal(copyErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	for _, secret := range []string{"source-rcon-secret", "source-server-secret", "rcon_password", "sv_password"} {
		if bytes.Contains(archiveContents.Bytes(), []byte(secret)) {
			t.Fatalf("backup archive contains secret material %q", secret)
		}
	}
}

func TestBackupManifestV1RemainsReadableAndRestorable(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	config := filepath.Join(source, "left4dead2", "cfg", "server.cfg")
	if err := os.MkdirAll(filepath.Dir(config), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("hostname legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "legacy-v1.zip")
	created, err := CreateBackup("legacy-source", source, archive)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != backupLegacyFormatVersion || created.Launch != nil {
		t.Fatalf("unexpected legacy manifest: %#v", created)
	}
	manifest, skipped, err := RestoreBackup(target, archive, filepath.Join(t.TempDir(), "restore-legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != backupLegacyFormatVersion || len(skipped) != 0 {
		t.Fatalf("legacy restore = %#v, skipped=%#v", manifest, skipped)
	}
}

func TestBackupV3StoresMapReferencesAndMapVPKBytes(t *testing.T) {
	t.Parallel()
	instancePath := t.TempDir()
	mapRelative := "left4dead2/addons/workshop/l4d2panel-map-11111111-1111-1111-1111-111111111111-00.vpk"
	mapPath := filepath.Join(instancePath, filepath.FromSlash(mapRelative))
	if err := os.MkdirAll(filepath.Dir(mapPath), 0o750); err != nil {
		t.Fatal(err)
	}
	mapObject := filepath.Join(t.TempDir(), "map-object.vpk")
	if err := os.WriteFile(mapObject, []byte("map backup content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(mapObject, mapPath); err != nil {
		t.Fatal(err)
	}
	launch := BackupLaunchConfig{Mode: "coop", StartMap: "custom_map", MaxPlayers: 8}
	reference := BackupMapReference{
		ID: "11111111-1111-1111-1111-111111111111", AggregateSHA256: strings.Repeat("a", 64), Name: "Custom Map",
		BSPs: []string{"maps/custom_map.bsp"},
	}
	archive := filepath.Join(t.TempDir(), "portable-v3.zip")
	created, err := CreateBackupWithMaps("source-instance", instancePath, archive, launch, []BackupMapReference{reference})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadBackupManifest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != backupFormatVersion || manifest.Version != backupFormatVersion || !reflect.DeepEqual(manifest.Maps, []BackupMapReference{reference}) {
		t.Fatalf("unexpected v3 map references: created=%#v manifest=%#v", created.Maps, manifest.Maps)
	}
	if _, included := manifest.Files[mapRelative]; !included {
		t.Fatal("assigned map VPK bytes were not included in backup")
	}
	beforeIdentity, _, err := managedHardLinkIdentity(instancePath, mapRelative)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RestoreBackup(instancePath, archive, filepath.Join(t.TempDir(), "restore-existing-map-link")); err != nil {
		t.Fatal(err)
	}
	afterIdentity, _, err := managedHardLinkIdentity(instancePath, mapRelative)
	if err != nil {
		t.Fatal(err)
	}
	if afterIdentity != beforeIdentity {
		t.Fatal("restore replaced an existing matching map hard link")
	}
	restoredPath := t.TempDir()
	if _, _, err := RestoreBackup(restoredPath, archive, filepath.Join(t.TempDir(), "restore-map-vpk")); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(restoredPath, filepath.FromSlash(mapRelative))); err != nil || string(raw) != "map backup content" {
		t.Fatalf("restored map VPK = %q, %v", raw, err)
	}
}

func TestBackupManifestV2RejectsSecretAndUnsafeLaunchFields(t *testing.T) {
	t.Parallel()
	valid := BackupManifest{
		Version: backupFormatVersion, InstanceID: "source", OS: "windows", Arch: "amd64",
		CreatedAt: time.Now().UTC(), Files: map[string]string{},
		Launch: &BackupLaunchConfig{Mode: "coop", StartMap: "c1m1_hotel", MaxPlayers: 8},
	}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["launch"].(map[string]any)["server_password"] = "must-not-be-representable"
	raw, _ = json.Marshal(object)
	if _, err := decodeBackupManifest(raw); err == nil {
		t.Fatal("manifest accepted a secret launch field")
	}

	valid.Launch.ExtraArgs = []string{"-basedir", "C:/escape"}
	raw, _ = json.Marshal(valid)
	if _, err := decodeBackupManifest(raw); err == nil {
		t.Fatal("manifest accepted unsafe startup arguments")
	}
	duplicateVersion := []byte(`{"version":1,"version":2,"instance_id":"source","os":"windows","arch":"amd64","created_at":"2026-08-01T00:00:00Z","files":{},"launch":{"mode":"coop","max_players":8,"start_map":"c1m1_hotel","auto_restart":false}}`)
	if _, err := decodeBackupManifest(duplicateVersion); err == nil {
		t.Fatal("manifest accepted duplicate JSON keys")
	}
	wrongCase := []byte(`{"Version":1,"instance_id":"source","os":"windows","arch":"amd64","created_at":"2026-08-01T00:00:00Z","files":{}}`)
	if _, err := decodeBackupManifest(wrongCase); err == nil {
		t.Fatal("manifest accepted non-canonical field names")
	}
	valid.Launch.ExtraArgs = nil
	valid.Launch.CVars = map[string]string{"hostname": "shadowed"}
	raw, _ = json.Marshal(valid)
	if _, err := decodeBackupManifest(raw); err == nil {
		t.Fatal("manifest accepted a CVar that collides with a structured field")
	}
}

func TestReadBackupManifestRejectsDuplicateManifest(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "duplicate.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for index := 0; index < 2; index++ {
		entry, createErr := writer.Create("manifest.json")
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, _ = entry.Write([]byte(`{"version":1}`))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// ReadBackupManifest rejects invalid content even if a ZIP reader chooses
	// the first duplicate; RestoreBackup additionally validates archive shape.
	if _, err := ReadBackupManifest(archive); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate/invalid manifest was accepted: %v", err)
	}
}

func TestReadBackupManifestRejectsOversizedManifest(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "oversized.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(bytes.Repeat([]byte{' '}, maxBackupManifestBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBackupManifest(archive); err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
		t.Fatalf("oversized manifest was accepted: %v", err)
	}
}
