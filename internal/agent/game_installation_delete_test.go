package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func newGameInstallationDeleteExecutor(t *testing.T, installationID, installationPath string) (*Executor, *runtimeEnvironmentStore) {
	t.Helper()
	dataDir := t.TempDir()
	configuredRoot := t.TempDir()
	store, err := newRuntimeEnvironmentStore(&Config{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations[installationID] = runtimeGameRecord{Path: installationPath}
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{configuredRoot})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{config: &Config{DataDir: dataDir}, paths: paths, registry: registry, runtimeEnvironment: store}, store
}

func gameInstallationDeleteJob(installationID, installationPath, mode string) domain.Job {
	return domain.Job{InstallationID: installationID, Payload: map[string]any{
		"installation_id": installationID,
		"path":            installationPath,
		"mode":            mode,
	}}
}

func TestDeleteGameInstallationRecordPreservesFiles(t *testing.T) {
	installationPath := filepath.Join(t.TempDir(), "server")
	if err := os.MkdirAll(installationPath, 0o750); err != nil {
		t.Fatal(err)
	}
	executor, store := newGameInstallationDeleteExecutor(t, "installation-one", installationPath)
	result, err := executor.deleteGameInstallation(gameInstallationDeleteJob("installation-one", installationPath, "record"), func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["files_deleted"] != false {
		t.Fatalf("record deletion result = %#v", result)
	}
	if _, err := os.Stat(installationPath); err != nil {
		t.Fatalf("record-only deletion removed files: %v", err)
	}
	if _, ok := store.snapshot().Installations["installation-one"]; ok {
		t.Fatal("record-only deletion left the Agent registration behind")
	}
}

func TestDeleteGameInstallationFilesRemovesExactRegisteredTree(t *testing.T) {
	installationPath := filepath.Join(t.TempDir(), "server")
	if err := os.MkdirAll(filepath.Join(installationPath, "left4dead2", "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installationPath, "left4dead2", "cfg", "server.cfg"), []byte("hostname test"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, store := newGameInstallationDeleteExecutor(t, "installation-one", installationPath)
	result, err := executor.deleteGameInstallation(gameInstallationDeleteJob("installation-one", installationPath, "files"), func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["files_deleted"] != true {
		t.Fatalf("file deletion result = %#v", result)
	}
	if _, err := os.Stat(installationPath); !os.IsNotExist(err) {
		t.Fatalf("installation tree still exists: %v", err)
	}
	if _, ok := store.snapshot().Installations["installation-one"]; ok {
		t.Fatal("file deletion left the Agent registration behind")
	}
}

func TestDeleteGameInstallationRejectsMismatchedOrBoundPath(t *testing.T) {
	installationPath := filepath.Join(t.TempDir(), "server")
	if err := os.MkdirAll(installationPath, 0o750); err != nil {
		t.Fatal(err)
	}
	executor, _ := newGameInstallationDeleteExecutor(t, "installation-one", installationPath)
	if _, err := executor.deleteGameInstallation(gameInstallationDeleteJob("installation-one", filepath.Join(t.TempDir(), "other"), "record"), func(int, string) {}); err == nil {
		t.Fatal("mismatched deletion path was accepted")
	}
	if err := executor.registry.Put(InstanceRecord{ID: "instance-one", RelativePath: installationPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.deleteGameInstallation(gameInstallationDeleteJob("installation-one", installationPath, "files"), func(int, string) {}); err == nil {
		t.Fatal("bound game installation was deleted")
	}
	if _, err := os.Stat(installationPath); err != nil {
		t.Fatalf("rejected deletion changed the installation: %v", err)
	}
}

func TestDeleteGameInstallationFilesProtectsNestedRegistrations(t *testing.T) {
	installationPath := filepath.Join(t.TempDir(), "server")
	nestedPath := filepath.Join(installationPath, "nested-server")
	if err := os.MkdirAll(nestedPath, 0o750); err != nil {
		t.Fatal(err)
	}
	executor, store := newGameInstallationDeleteExecutor(t, "installation-one", installationPath)
	if err := store.update(func(state *runtimeEnvironmentState) {
		state.Installations["installation-two"] = runtimeGameRecord{Path: nestedPath}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.deleteGameInstallation(gameInstallationDeleteJob("installation-one", installationPath, "files"), func(int, string) {}); err == nil {
		t.Fatal("parent installation containing another registration was deleted")
	}
	if _, err := os.Stat(nestedPath); err != nil {
		t.Fatalf("rejected deletion touched the nested installation: %v", err)
	}
}

func TestDeleteGameInstallationFilesRejectsLinkedTree(t *testing.T) {
	installationPath := filepath.Join(t.TempDir(), "server")
	if err := os.MkdirAll(installationPath, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(installationPath, "linked")); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	executor, _ := newGameInstallationDeleteExecutor(t, "installation-one", installationPath)
	if _, err := executor.deleteGameInstallation(gameInstallationDeleteJob("installation-one", installationPath, "files"), func(int, string) {}); err == nil {
		t.Fatal("linked installation tree was deleted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("rejected deletion touched the link target: %v", err)
	}
}
