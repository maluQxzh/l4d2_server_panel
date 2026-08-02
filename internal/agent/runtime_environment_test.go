package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestValidateSteamCMDPathRejectsUnsafeLocationsBeforeExecution(t *testing.T) {
	root := t.TempDir()
	launcher := "steamcmd.sh"
	if runtime.GOOS == "windows" {
		launcher = "steamcmd.exe"
	}
	inside := filepath.Join(root, launcher)
	if _, err := validateSteamCMDPath(context.Background(), inside, []string{root}); err == nil {
		t.Fatal("SteamCMD inside a configured game root was accepted")
	}
	if _, err := validateSteamCMDPath(context.Background(), "relative/"+launcher, []string{root}); err == nil {
		t.Fatal("relative SteamCMD path was accepted")
	}
	wrongName := filepath.Join(filepath.Dir(root), "not-steamcmd")
	if runtime.GOOS == "windows" {
		wrongName += ".exe"
	}
	if _, err := validateSteamCMDPath(context.Background(), wrongName, []string{root}); err == nil {
		t.Fatal("unsupported SteamCMD launcher name was accepted")
	}
}

func TestRuntimeEnvironmentStateUpdatePersistsAtomically(t *testing.T) {
	data := t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.SteamCMDPath = filepath.Join(data, "steamcmd", "steamcmd")
		state.SteamCMDSource = "managed"
		state.Installations["installation-one"] = runtimeGameRecord{RootIndex: 2, RelativePath: "servers/one"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations["installation-two"] = runtimeGameRecord{RootIndex: 3, RelativePath: "servers/two"}
	}); err != nil {
		t.Fatalf("replace existing runtime state: %v", err)
	}
	reloaded, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	state := reloaded.snapshot()
	if state.SteamCMDSource != "managed" || state.Installations["installation-one"].RelativePath != "servers/one" || state.Installations["installation-two"].RelativePath != "servers/two" {
		t.Fatalf("unexpected persisted state: %#v", state)
	}
}

func TestConfigureSteamCMDFailureKeepsActivePath(t *testing.T) {
	data, gameRoot := t.TempDir(), t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(data, "previous", "steamcmd")
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.SteamCMDPath, state.SteamCMDSource = oldPath, "custom"
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{gameRoot})
	if err != nil {
		t.Fatal(err)
	}
	launcher := "steamcmd.sh"
	if runtime.GOOS == "windows" {
		launcher = "steamcmd.exe"
	}
	executor := &Executor{config: &Config{DataDir: data, SteamCMDConcurrency: 1}, paths: paths, runtimeEnvironment: store}
	_, err = executor.configureSteamCMD(context.Background(), domain.Job{Payload: map[string]any{"path": filepath.Join(data, "missing", launcher)}}, "custom", func(int, string) {})
	if err == nil {
		t.Fatal("missing replacement SteamCMD was accepted")
	}
	state := store.snapshot()
	if state.SteamCMDPath != oldPath || state.SteamCMDSource != "custom" {
		t.Fatalf("failed replacement changed active SteamCMD: %#v", state)
	}
}

func TestGameMaintenanceRejectsUnregisteredInstallation(t *testing.T) {
	data, gameRoot := t.TempDir(), t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{gameRoot})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{config: &Config{DataDir: data, SteamCMDConcurrency: 1}, paths: paths, runtimeEnvironment: store}
	_, err = executor.gameInstallationAction(context.Background(), domain.Job{
		InstallationID: "installation-missing",
		Payload:        map[string]any{"root_index": 0, "path": "servers/missing"},
	}, "game.update", func(int, string) {})
	if err == nil || err.Error() != "game installation is not registered on this Agent" {
		t.Fatalf("unregistered installation was not rejected: %v", err)
	}
}

func TestValidateSteamCMDPathLinuxPermissionsAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux launcher permission semantics")
	}
	directory, gameRoot := t.TempDir(), t.TempDir()
	launcher := filepath.Join(directory, "steamcmd.sh")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = '+quit' ]\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if version, err := validateSteamCMDPath(context.Background(), launcher, []string{gameRoot}); err != nil || version != "available" {
		t.Fatalf("valid launcher rejected: version=%q err=%v", version, err)
	}
	if err := os.Chmod(launcher, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateSteamCMDPath(context.Background(), launcher, []string{gameRoot}); err == nil {
		t.Fatal("non-executable launcher was accepted")
	}
	if err := os.Chmod(launcher, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(directory, "steamcmd")
	if err := os.Symlink(launcher, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := validateSteamCMDPath(context.Background(), linked, []string{gameRoot}); err == nil {
		t.Fatal("linked launcher was accepted")
	}
}
