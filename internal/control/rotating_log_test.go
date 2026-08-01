package control

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingLogBoundsFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.jsonl")
	writer, err := NewRotatingLog(path, 16, 2)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 6; index++ {
		if _, err := writer.Write([]byte("0123456789\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		if _, err := os.Stat(candidate); err != nil {
			t.Fatalf("expected bounded log file %s: %v", candidate, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected unbounded backup: %v", err)
	}
}
