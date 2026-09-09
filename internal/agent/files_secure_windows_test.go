//go:build windows

package agent

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedWindowsRootOpensOrdinaryDOSPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "节点 data", "nested")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, `\\?\` + root} {
		for _, write := range []bool{false, true} {
			handle, err := openManagedRootWindows(path, write)
			if err != nil {
				t.Fatalf("open ordinary directory (write=%v): %v", write, err)
			}
			_ = windows.CloseHandle(handle)
		}
	}
}

func TestManagedWindowsRootRejectsReparseComponents(t *testing.T) {
	for _, kind := range []string{"junction", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(base, "real")
			if err := os.MkdirAll(filepath.Join(target, "data"), 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(base, "redirect")
			if kind == "junction" {
				createManagedWindowsTestJunction(t, link, target)
			} else if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			for _, root := range []string{link, filepath.Join(link, "data")} {
				for _, write := range []bool{false, true} {
					handle, err := openManagedRootWindows(root, write)
					if err == nil {
						_ = windows.CloseHandle(handle)
					}
					if !errors.Is(err, ErrPathEscape) {
						t.Fatalf("reparse root (write=%v) returned %v, want ErrPathEscape", write, err)
					}
				}
			}
			// Even an absent first-install state file must not hide a root escape.
			if _, err := LoadRuntimeServicePaths(filepath.Join(link, "data")); !errors.Is(err, ErrPathEscape) {
				t.Fatalf("service paths accepted a reparse ancestor: %v", err)
			}
		})
	}
}

func createManagedWindowsTestJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Mkdir(link, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	// REPARSE_DATA_BUFFER with a mount-point payload; lengths exclude NULs.
	buffer := make([]byte, 16+2*(len(substitute)+len(printName)))
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(len(buffer)-8))
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(2*(len(printName)-1)))
	for i, value := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], value)
	}
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil); err != nil {
		t.Fatal(err)
	}
}

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
