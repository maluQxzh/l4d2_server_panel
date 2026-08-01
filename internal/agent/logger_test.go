package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotatingLogPrunesDataOlderThanRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.log")
	if err := os.WriteFile(path, []byte("old current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	logFile, err := NewRotatingLogWithRetention(path, 1024, 2, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	if _, err := logFile.Write([]byte("fresh\n")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "fresh\n" {
		t.Fatalf("current log = %q, want only fresh data", raw)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expired rotated log remains: %v", err)
	}
}
