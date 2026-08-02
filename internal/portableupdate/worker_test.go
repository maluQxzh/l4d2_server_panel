package portableupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunWorkerSuccessfulUpgradePreservesDatabaseAndManagedBoundaries(t *testing.T) {
	manager, jobID := newWorkerFixture(t, false)
	if err := runWorker(manager, jobID); err != nil {
		t.Fatalf("run successful worker: %v", err)
	}
	assertFileBody(t, filepath.Join(manager.Root, "l4d2panel-control.exe"), "new-control")
	assertFileBody(t, filepath.Join(manager.Root, "migrations", "sqlite", "001.sql"), "new-migration")
	assertFileBody(t, filepath.Join(manager.Root, "new-release.txt"), "new-release")
	assertNotExist(t, filepath.Join(manager.Root, "old-release.txt"))
	assertFileBody(t, manager.DatabasePath, "migrated-database")
	assertFileBody(t, manager.ConfigPath, "old-config")
	state, err := manager.State(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "succeeded" || state.Phase != "health_check" || state.Rollback != "not_required" || state.CompletedAt == nil {
		t.Fatalf("unexpected successful state: %#v", state)
	}
	assertNotExist(t, manager.activePath())
	assertNotExist(t, filepath.Join(manager.jobDir(jobID), "package.zip"))
	assertFileBody(t, filepath.Join(manager.snapshotDir(jobID), "snapshot.complete"), "complete\n")
}

func TestRunWorkerMigrationFailureRestoresProgramConfigAndDatabase(t *testing.T) {
	manager, jobID := newWorkerFixture(t, true)
	if err := runWorker(manager, jobID); err == nil {
		t.Fatal("expected injected migration failure")
	}
	assertFileBody(t, filepath.Join(manager.Root, "l4d2panel-control.exe"), "old-control")
	assertFileBody(t, filepath.Join(manager.Root, "migrations", "sqlite", "001.sql"), "old-migration")
	assertFileBody(t, filepath.Join(manager.Root, "old-release.txt"), "old-release")
	assertNotExist(t, filepath.Join(manager.Root, "new-release.txt"))
	assertFileBody(t, manager.DatabasePath, "old-database")
	assertFileBody(t, manager.ConfigPath, "old-config")
	manifest, err := loadManifest(filepath.Join(manager.Root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1.0.0" {
		t.Fatalf("rollback restored manifest version %q", manifest.Version)
	}
	state, err := manager.State(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "failed" || state.Phase != "rolling_back" || state.Rollback != "succeeded" || state.CompletedAt == nil {
		t.Fatalf("unexpected rollback state: %#v", state)
	}
	if state.Error == "" {
		t.Fatal("failed state did not retain a sanitized diagnostic")
	}
	assertNotExist(t, manager.activePath())
	assertNotExist(t, filepath.Join(manager.jobDir(jobID), "package.zip"))
}

func TestRunWorkerRetainsDiagnosticsWhenRollbackCannotRestartService(t *testing.T) {
	manager, jobID := newWorkerFixture(t, true)
	baseCommand := manager.command
	manager.command = func(executable string, args ...string) error {
		if len(args) >= 2 && args[0] == "service" && args[1] == "start" {
			return errors.New("injected service restart failure")
		}
		return baseCommand(executable, args...)
	}
	if err := runWorker(manager, jobID); err == nil {
		t.Fatal("expected update and rollback failure")
	}
	state, err := manager.State(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "failed" || state.Rollback != "failed" || state.Phase != "rolling_back" {
		t.Fatalf("unexpected rollback-failure state: %#v", state)
	}
	if _, err := os.Stat(filepath.Join(manager.jobDir(jobID), "package.zip")); err != nil {
		t.Fatalf("failed rollback did not retain the package: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.jobDir(jobID), "staging")); err != nil {
		t.Fatalf("failed rollback did not retain staging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.snapshotDir(jobID), "snapshot.complete")); err != nil {
		t.Fatalf("failed rollback did not retain the snapshot: %v", err)
	}
	assertNotExist(t, manager.activePath())
}

func TestPortableDiagnosticRedactsPathsAndSetupKeys(t *testing.T) {
	manager := &Manager{Root: `C:\center`, ConfigPath: `C:\center\data\config\control.json`, DatabasePath: `C:\center\data\database\l4d2panel.sqlite3`, ExecutablePath: `C:\center\l4d2panel-control.exe`}
	setupKey := "l4d2p_setup_abcdefghijklmnopqrstuvwxyz0123456789_-ABCDE"
	if len(setupKey) != len("l4d2p_setup_")+43 {
		t.Fatalf("test setup key length=%d", len(setupKey))
	}
	diagnostic := manager.sanitizedError(errors.New(`replace C:\CENTER\data\database\l4d2panel.sqlite3 failed for ` + setupKey))
	if strings.Contains(strings.ToLower(diagnostic), `c:\center`) || strings.Contains(diagnostic, setupKey) {
		t.Fatalf("diagnostic was not redacted: %q", diagnostic)
	}
}

func newWorkerFixture(t *testing.T, failMigration bool) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "data", "config", "control.json")
	databasePath := filepath.Join(root, "data", "database", "l4d2panel.sqlite3")
	writeTestFile(t, configPath, "old-config")
	writeTestFile(t, databasePath, "old-database")
	writeManagedTree(t, root, "1.0.0", map[string]string{
		"l4d2panel-control.exe":     "old-control",
		"migrations/sqlite/001.sql": "old-migration",
		"old-release.txt":           "old-release",
	})
	jobID := uuid.NewString()
	manager := &Manager{
		Root: root, ConfigPath: configPath, DatabasePath: databasePath,
		CurrentVersion: "1.0.0", ExecutablePath: filepath.Join(root, "l4d2panel-control.exe"), ServiceMode: true,
		ready: func(time.Duration) error { return nil },
	}
	manager.command = func(_ string, args ...string) error {
		if len(args) == 0 {
			return errors.New("missing command")
		}
		switch args[0] {
		case "backup-database":
			for index := 1; index+1 < len(args); index++ {
				if args[index] == "--output" {
					return copyRegularFile(databasePath, args[index+1], 0o600)
				}
			}
			return errors.New("backup output was not provided")
		case "migrate":
			if err := os.WriteFile(databasePath, []byte("migrated-database"), 0o600); err != nil {
				return err
			}
			if failMigration {
				return errors.New("injected migration failure")
			}
		}
		return nil
	}
	jobDir := manager.jobDir(jobID)
	writeManagedTree(t, filepath.Join(jobDir, "staging"), "1.1.0", map[string]string{
		"l4d2panel-control.exe":     "new-control",
		"migrations/sqlite/001.sql": "new-migration",
		"new-release.txt":           "new-release",
	})
	writeTestFile(t, filepath.Join(jobDir, "package.zip"), "package")
	now := time.Now().UTC()
	if err := writeState(manager.statePath(jobID), State{
		JobID: jobID, Status: "queued", Phase: "validating", CurrentVersion: "1.0.0", TargetVersion: "1.1.0",
		PackageSHA256: "0123456789abcdef", UploadBytes: 7, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.claimActive(jobID); err != nil {
		t.Fatal(err)
	}
	return manager, jobID
}

func writeManagedTree(t *testing.T, root, version string, files map[string]string) {
	t.Helper()
	manifest := Manifest{FormatVersion: portableFormat, Product: portableProduct, Platform: portablePlatform, Architecture: portableArchitecture, Version: version}
	for path, body := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(path)), body)
		digest := sha256.Sum256([]byte(body))
		manifest.Files = append(manifest.Files, ManifestFile{Path: path, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])})
	}
	if err := writeJSONAtomic(filepath.Join(root, ManifestName), manifest); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, filename, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileBody(t *testing.T, filename, expected string) {
	t.Helper()
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	if string(body) != expected {
		t.Fatalf("%s contains %q, expected %q", filename, body, expected)
	}
}

func assertNotExist(t *testing.T, filename string) {
	t.Helper()
	if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s not to exist, got %v", filename, err)
	}
}
