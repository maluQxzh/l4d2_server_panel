//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagedLinuxParentHandleSurvivesNamespaceReplacement(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "cfg", "live")
	moved := filepath.Join(root, "cfg", "original")
	outside := t.TempDir()
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "server.cfg")
	if err := os.WriteFile(outsideFile, []byte("outside-sentinel"), 0o640); err != nil {
		t.Fatal(err)
	}
	parent, leaf, err := openManagedParentLinux(root, "cfg/live/server.cfg", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parent)
	if err := os.Rename(live, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, live); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedFileAtLinux(parent, leaf, []byte("anchored")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outsideFile)
	if err != nil || string(raw) != "outside-sentinel" {
		t.Fatalf("outside file was changed: %q, %v", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(moved, leaf))
	if err != nil || string(raw) != "anchored" {
		t.Fatalf("write did not stay on the anchored directory: %q, %v", raw, err)
	}
}
