package agent

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessManagerCleanupLogsPrunesIdleAndRotatedSegments(t *testing.T) {
	manager, err := NewProcessManager(t.TempDir(), log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	writeOld := func(name string) {
		t.Helper()
		path := filepath.Join(manager.logDir, name)
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	writeOld("idle.log")
	writeOld("active.log")
	writeOld("active.log.1")
	writeOld("unrelated.txt")
	manager.items["active"] = &managedProcess{cmd: &exec.Cmd{}}

	removed, err := manager.CleanupLogs(time.Now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d logs, want idle current plus active rotated segment", removed)
	}
	for _, name := range []string{"idle.log", "active.log.1"} {
		if _, err := os.Stat(filepath.Join(manager.logDir, name)); !os.IsNotExist(err) {
			t.Fatalf("expired log %q remains: %v", name, err)
		}
	}
	for _, name := range []string{"active.log", "unrelated.txt"} {
		if _, err := os.Stat(filepath.Join(manager.logDir, name)); err != nil {
			t.Fatalf("protected log %q was removed: %v", name, err)
		}
	}
}
