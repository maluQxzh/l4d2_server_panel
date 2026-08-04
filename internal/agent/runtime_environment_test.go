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
		state.Installations["installation-one"] = runtimeGameRecord{Path: filepath.Join(data, "servers", "one")}
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations["installation-two"] = runtimeGameRecord{Path: filepath.Join(data, "servers", "two")}
	}); err != nil {
		t.Fatalf("replace existing runtime state: %v", err)
	}
	reloaded, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	state := reloaded.snapshot()
	if state.SteamCMDSource != "managed" || state.Installations["installation-one"].Path != filepath.Join(data, "servers", "one") || state.Installations["installation-two"].Path != filepath.Join(data, "servers", "two") {
		t.Fatalf("unexpected persisted state: %#v", state)
	}
}

func TestLoadRuntimeServicePathsReturnsOnlyAbsoluteLocalPaths(t *testing.T) {
	data := t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(data, "servers", "one")
	second := filepath.Join(data, "servers", "two")
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations["two"] = runtimeGameRecord{Path: second}
		state.Installations["one"] = runtimeGameRecord{Path: first}
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := LoadRuntimeServicePaths(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths.InstallationPaths) != 2 || paths.InstallationPaths[0] != first || paths.InstallationPaths[1] != second {
		t.Fatalf("unexpected runtime service paths: %#v", paths)
	}
}

func TestRuntimeEnvironmentStoreRemovesLegacyRelativeInstallationPaths(t *testing.T) {
	data := t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations["legacy-relative"] = runtimeGameRecord{Path: "servers/legacy"}
		state.Installations["absolute"] = runtimeGameRecord{Path: filepath.Join(data, "servers", "absolute")}
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	state := reloaded.snapshot()
	if _, ok := state.Installations["legacy-relative"]; ok {
		t.Fatal("legacy relative runtime installation survived upgrade")
	}
	if _, ok := state.Installations["absolute"]; !ok {
		t.Fatal("absolute runtime installation was removed")
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
		Payload:        map[string]any{"path": filepath.Join(gameRoot, "servers", "missing")},
	}, "game.update", func(int, string) {})
	if err == nil || err.Error() != "game installation is not registered on this Agent" {
		t.Fatalf("unregistered installation was not rejected: %v", err)
	}
}

func TestBindRegisteredInstallationSkipsRepeatedStructureValidation(t *testing.T) {
	data, configuredRoot, installationPath := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations["installation-one"] = runtimeGameRecord{Path: installationPath}
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(installationPath, "left4dead2", "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{configuredRoot})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(data)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{
		config: &Config{DataDir: data}, paths: paths, registry: registry, runtimeEnvironment: store,
		processes: &ProcessManager{items: make(map[string]*managedProcess)},
	}
	job := domain.Job{ID: "job-one", InstanceID: "instance-one", InstallationID: "installation-one", Payload: map[string]any{
		"operation": "bind", "path": installationPath, "rcon_password": "secret",
		"instance": map[string]any{"id": "instance-one", "name": "one", "path": installationPath, "port": 27015,
			"config": map[string]any{"hostname": "one", "mode": "mutation4", "difficulty": "normal", "max_players": 4,
				"start_map": "c1m1_hotel", "region": "255", "extra_args": []any{"-tickrate", "100"}}},
	}}
	if _, err := executor.importInstance(context.Background(), job, func(int, string) {}); err != nil {
		t.Fatalf("registered installation binding repeated structure validation: %v", err)
	}
	if _, ok := registry.Get("instance-one"); !ok {
		t.Fatal("bound instance was not registered")
	}
	if _, err := os.Stat(filepath.Join(installationPath, "left4dead2", "cfg", "l4d2panel.cfg")); !os.IsNotExist(err) {
		t.Fatalf("l4d2panel.cfg should not be generated: %v", err)
	}
}

func TestGameImportChecksOnlySrcdsAtAbsolutePath(t *testing.T) {
	data, configuredRoot, installationPath := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{configuredRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installationPath, defaultServerExecutable()), []byte("srcds"), 0o700); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{config: &Config{DataDir: data, SteamCMDConcurrency: 1}, paths: paths, runtimeEnvironment: store}
	result, err := executor.gameInstallationAction(context.Background(), domain.Job{
		InstallationID: "installation-absolute",
		Payload:        map[string]any{"path": installationPath},
	}, "game.import", func(int, string) {})
	if err != nil {
		t.Fatalf("absolute installation with srcds was rejected: %v", err)
	}
	if result["path"] != installationPath {
		t.Fatalf("result path = %#v, want %q", result["path"], installationPath)
	}
	if record := store.snapshot().Installations["installation-absolute"]; record.Path != installationPath {
		t.Fatalf("runtime installation path = %q, want %q", record.Path, installationPath)
	}
	if _, err := os.Stat(filepath.Join(installationPath, "left4dead2", "gameinfo.txt")); !os.IsNotExist(err) {
		t.Fatalf("test unexpectedly supplied gameinfo.txt: %v", err)
	}
}

func TestValidateServiceGameInstallationRequiresExactPlainRoot(t *testing.T) {
	parent := t.TempDir()
	installationPath := filepath.Join(parent, "server")
	if err := os.MkdirAll(installationPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installationPath, defaultServerExecutable()), []byte("srcds"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := ValidateServiceGameInstallation(installationPath); err != nil || got != installationPath {
		t.Fatalf("valid service game installation = %q, %v", got, err)
	}
	if _, err := ValidateServiceGameInstallation(parent); err == nil {
		t.Fatal("parent of a game installation was accepted for service adoption")
	}
	linked := filepath.Join(parent, "linked")
	if err := os.Symlink(installationPath, linked); err == nil {
		if _, err := ValidateServiceGameInstallation(linked); err == nil {
			t.Fatal("linked game installation was accepted for service adoption")
		}
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
