package agent

import (
	"path/filepath"
	"testing"
)

func TestAgentDataDirectoryIsExclusivelyLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".agent.lock")
	first, err := acquireAgentDataLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireAgentDataLock(path); err == nil {
		_ = second.Close()
		t.Fatal("a second Agent acquired the same data_dir lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireAgentDataLock(path)
	if err != nil {
		t.Fatalf("data_dir lock was not released: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}
