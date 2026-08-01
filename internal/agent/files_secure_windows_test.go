//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedWindowsParentHandlePinsDirectory(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "cfg", "live")
	moved := filepath.Join(root, "cfg", "moved")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	handle, leaf, err := openManagedParentWindows(root, "cfg/live/server.cfg", true)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = windows.CloseHandle(handle)
		}
	}()

	if err := os.Rename(live, moved); err == nil {
		t.Fatal("an opened managed parent could be renamed despite the pinned handle")
	}
	if err := writeManagedFileAtWindows(handle, leaf, []byte("anchored")); err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := os.Rename(live, moved); err != nil {
		t.Fatalf("directory remained pinned after handle close: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(moved, leaf))
	if err != nil || string(raw) != "anchored" {
		t.Fatalf("anchored write missing: %q, %v", raw, err)
	}
}

func TestManagedWindowsRejectsADSAndDeviceNames(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{
		"cfg/server.cfg:secret",
		"cfg/CON.foo.bar",
		"cfg/nul.cfg",
		"cfg/LPT9.any.extension",
	} {
		if err := WriteManagedFile(root, relative, "blocked", ""); err == nil {
			t.Fatalf("Windows special path %q was accepted", relative)
		}
	}
}
