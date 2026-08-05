package agent

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestRestoreV2UsesSourceLaunchConfigAndTargetSecrets(t *testing.T) {
	executor, dataDir, targetPath := uploadedPackageExecutor(t)
	processes, err := NewProcessManager(dataDir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	executor.processes = processes

	targetArgs, err := BuildStartArgs(StartOptions{
		Port: 27019, TVPort: 27025, MaxPlayers: 4, Map: "c2m1_highway", Mode: "coop",
	})
	if err != nil {
		t.Fatal(err)
	}
	record, _ := executor.registry.Get("instance")
	record.Name, record.Args, record.RCONHost, record.RCONPort, record.TVPort = "Target", targetArgs, "127.0.0.1", 27019, 27025
	record.AutoRestart = false
	if err := executor.registry.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := writeGeneratedServerConfig(targetPath, []byte("hostname \"Target hostname\"\nrcon_password \"target-old-rcon\"\n")); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(dataDir, "source-instance")
	if err := os.MkdirAll(filepath.Join(sourcePath, "left4dead2", "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "left4dead2", "cfg", "server.cfg"), []byte("// source portable file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceLaunch := BackupLaunchConfig{
		Hostname: "Restored source hostname", Mode: "scavenge", Difficulty: "impossible", MaxPlayers: 16,
		StartMap: "c7m1_docks", Region: "3", SteamGroupID: "987654321", AutoRestart: true,
		ExtraArgs: []string{"-secure", "+sv_tags", "restored"}, CVars: map[string]string{"sv_consistency": "1"},
	}
	archiveDir := filepath.Join(dataDir, "imported-backups")
	if err := os.MkdirAll(archiveDir, 0o750); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(archiveDir, "source-v2.zip")
	if _, err := CreateBackupWithLaunch("another-node-instance", sourcePath, archive, sourceLaunch); err != nil {
		t.Fatal(err)
	}

	result, err := executor.restore(context.Background(), domain.Job{
		ID: "restore-source-launch", InstanceID: "instance", Type: "restore",
		Payload: map[string]any{
			"archive": archive, "rcon_password": "target-new-rcon", "server_password": "target-new-server-password",
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["launch_config_restored"] != true || result["format_version"] != backupLaunchFormatVersion {
		t.Fatalf("unexpected restore result: %#v", result)
	}
	projected, ok := result["restored_launch_config"].(*BackupLaunchConfig)
	if !ok || projected.Mode != sourceLaunch.Mode || projected.StartMap != sourceLaunch.StartMap {
		t.Fatalf("restore result omitted the safe Control projection: %#v", result["restored_launch_config"])
	}
	restoredRecord, ok := executor.registry.Get("instance")
	if !ok || !restoredRecord.AutoRestart || restoredRecord.RCONPort != 27019 || restoredRecord.TVPort != 27025 {
		t.Fatalf("target-local endpoints or source restart policy were not preserved: %#v", restoredRecord)
	}
	restoredLaunch, err := parsePortableStartArgs(restoredRecord.Args, 27019, 27025)
	if err != nil {
		t.Fatal(err)
	}
	if restoredLaunch.Mode != sourceLaunch.Mode || restoredLaunch.StartMap != sourceLaunch.StartMap || restoredLaunch.MaxPlayers != sourceLaunch.MaxPlayers || !reflect.DeepEqual(restoredLaunch.ExtraArgs, sourceLaunch.ExtraArgs) {
		t.Fatalf("registry uses the wrong launch configuration: %#v", restoredLaunch)
	}
	if _, err := os.Stat(filepath.Join(targetPath, "left4dead2", "cfg", "l4d2panel.cfg")); !os.IsNotExist(err) {
		t.Fatalf("legacy generated configuration was not removed: %v", err)
	}
	if restoredRecord.Config.Hostname != sourceLaunch.Hostname || restoredRecord.Config.Difficulty != sourceLaunch.Difficulty || !reflect.DeepEqual(restoredRecord.Config.CVars, sourceLaunch.CVars) {
		t.Fatalf("restored safe configuration was not persisted in the registry: %#v", restoredRecord.Config)
	}
}

func TestRestoreCapturedInstanceStateRestoresGeneratedConfigAndRegistry(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	original, _ := executor.registry.Get("instance")
	original.Args = []string{"-console", "-game", "left4dead2", "-port", "27015", "+map", "c1m1_hotel", "+mp_gamemode", "coop", "+maxplayers", "8"}
	if err := executor.registry.Put(original); err != nil {
		t.Fatal(err)
	}
	if err := writeGeneratedServerConfig(instancePath, []byte("rcon_password \"before\"\n")); err != nil {
		t.Fatal(err)
	}
	state, err := captureRestoreRollbackState(original, instancePath)
	if err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.AutoRestart = true
	changed.Args = append(changed.Args, "-secure")
	if err := executor.registry.Put(changed); err != nil {
		t.Fatal(err)
	}
	if err := writeGeneratedServerConfig(instancePath, []byte("rcon_password \"after\"\n")); err != nil {
		t.Fatal(err)
	}
	if err := executor.restoreCapturedInstanceState(state); err != nil {
		t.Fatal(err)
	}
	restored, _ := executor.registry.Get("instance")
	if restored.AutoRestart || !reflect.DeepEqual(restored.Args, original.Args) {
		t.Fatalf("registry was not rolled back: %#v", restored)
	}
	raw, err := os.ReadFile(filepath.Join(instancePath, "left4dead2", "cfg", "l4d2panel.cfg"))
	if err != nil || string(raw) != "rcon_password \"before\"\n" {
		t.Fatalf("generated config was not rolled back: %q, %v", raw, err)
	}
}
