package portableupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerSupportsDevelopmentBuilds(t *testing.T) {
	if !RuntimeSupported() {
		t.Skip("portable manager support is Windows x64 specific")
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "data", "config", "control.json")
	databasePath := filepath.Join(root, "data", "database", "l4d2panel.sqlite3")
	writeTestFile(t, filepath.Join(root, ManifestName), "{}")
	manager := &Manager{Root: root, ConfigPath: configPath, DatabasePath: databasePath, CurrentVersion: "dev", ServiceMode: true}
	if supported, reason := manager.Supported(); !supported {
		t.Fatalf("development portable build should support updates: %s", reason)
	}
}

func TestManagerCreateEnforcesIdempotencyAndSingleActiveUpdate(t *testing.T) {
	if !RuntimeSupported() {
		t.Skip("portable manager creation is Windows x64 specific")
	}
	root := t.TempDir()
	writeManagedTree(t, root, "1.0.0", map[string]string{
		"l4d2panel-control.exe":     "old-control",
		"migrations/sqlite/001.sql": "old-migration",
	})
	configPath := filepath.Join(root, "data", "config", "control.json")
	databasePath := filepath.Join(root, "data", "database", "l4d2panel.sqlite3")
	writeTestFile(t, configPath, "config")
	writeTestFile(t, databasePath, "database")
	manager := &Manager{
		Root: root, ConfigPath: configPath, DatabasePath: databasePath, CurrentVersion: "1.0.0",
		ExecutablePath: filepath.Join(root, "l4d2panel-control.exe"), ServiceMode: true,
		startWorker: func(_, _, _ string) error { return nil },
	}
	firstPackage := buildTestPackage(t, "1.1.0", map[string]string{
		"l4d2panel-control.exe":     "new-control",
		"migrations/sqlite/001.sql": "new-migration",
	})
	first := createFromPath(t, manager, firstPackage, "owner-key")
	replay := createFromPath(t, manager, firstPackage, "owner-key")
	if replay.JobID != first.JobID || !replay.IdempotentReplay {
		t.Fatalf("same package/key did not replay original task: first=%#v replay=%#v", first, replay)
	}

	differentPackage := buildTestPackage(t, "1.2.0", map[string]string{
		"l4d2panel-control.exe":     "different-control",
		"migrations/sqlite/001.sql": "newer-migration",
	})
	f, err := os.Open(differentPackage)
	if err != nil {
		t.Fatal(err)
	}
	_, conflictErr := manager.Create(f, "owner-key")
	f.Close()
	if !errors.Is(conflictErr, ErrIdempotencyConflict) {
		t.Fatalf("same key with different package returned %v", conflictErr)
	}

	f, err = os.Open(firstPackage)
	if err != nil {
		t.Fatal(err)
	}
	_, activeErr := manager.Create(f, "second-key")
	f.Close()
	if !errors.Is(activeErr, ErrUpdateActive) {
		t.Fatalf("second concurrent task returned %v", activeErr)
	}
	entries, err := os.ReadDir(filepath.Join(root, "data", "update", "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != first.JobID {
		t.Fatalf("rejected task left orphaned job directories: %#v", entries)
	}
}

func createFromPath(t *testing.T, manager *Manager, path, key string) State {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	state, err := manager.Create(f, key)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
