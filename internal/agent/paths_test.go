package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathGuardRejectsTraversalAndAbsolutePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instances")
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"../outside", filepath.Join("nested", "..", "..", "outside"), filepath.Join(filepath.VolumeName(root)+string(filepath.Separator), "outside")} {
		if _, err := guard.Resolve(0, value); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Resolve(%q) error = %v, want ErrPathEscape", value, err)
		}
	}
	path, err := guard.Resolve(0, filepath.Join("server-1", "left4dead2", "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if !isWithin(root, path) {
		t.Fatalf("resolved path %q is not within %q", path, root)
	}
}

func TestPathGuardRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "instances")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Resolve(0, filepath.Join("escape", "new", "file.cfg")); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("error = %v, want ErrPathEscape", err)
	}
}

func TestManagedFileBoundary(t *testing.T) {
	if _, err := cleanManagedRelative("left4dead2/cfg/server.cfg"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"srcds.exe", "left4dead2/bin/server.dll", "../cfg/server.cfg"} {
		if _, err := cleanManagedRelative(path); err == nil {
			t.Errorf("expected %q to be rejected", path)
		}
	}
}
