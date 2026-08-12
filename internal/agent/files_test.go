package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedFileReadIncludesDigestAndWriteUsesPrecondition(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "left4dead2", "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	relative := "left4dead2/cfg/server.cfg"
	if err := WriteManagedFile(root, relative, "hostname before\n", ""); err != nil {
		t.Fatal(err)
	}
	encoded, size, digest, err := ReadManagedFile(root, relative)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw := []byte("hostname before\n")
	wantDigest := sha256.Sum256(wantRaw)
	if size != int64(len(wantRaw)) || digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("unexpected metadata size=%d digest=%s", size, digest)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != string(wantRaw) {
		t.Fatalf("unexpected content %q: %v", decoded, err)
	}
	if err := verifyManagedFilePrecondition(root, relative, digest); err != nil {
		t.Fatalf("matching precondition failed: %v", err)
	}
	if err := WriteManagedFile(root, relative, "hostname changed\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := verifyManagedFilePrecondition(root, relative, digest); err == nil {
		t.Fatal("expected a stale digest to be rejected")
	}
}

func TestManagedFilesRejectUnmanagedPaths(t *testing.T) {
	root := t.TempDir()
	if err := WriteManagedFile(root, "../outside.txt", "blocked", ""); err == nil {
		t.Fatal("expected path escape to be rejected")
	}
	if err := WriteManagedFile(root, "left4dead2/bin/server.dll", "blocked", ""); err == nil {
		t.Fatal("expected unmanaged root to be rejected")
	}
}

func TestManagedFileMutationsRejectLoadBearingFiles(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{
		"left4dead2/addons/evil.vpk",
		"left4dead2/addons/sourcemod/plugins/evil.smx",
		"left4dead2/addons/sourcemod/extensions/evil.dll",
		"left4dead2/addons/sourcemod/extensions/evil.so",
		"left4dead2/addons/metamod.vdf",
	} {
		if err := WriteManagedFile(root, relative, "payload", ""); err == nil {
			t.Fatalf("WriteManagedFile(%q) accepted a load-bearing file", relative)
		}
	}
}

func TestManagedFileOperationsRefuseSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "left4dead2"), 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "server.cfg")
	if err := os.WriteFile(sentinel, []byte("outside-sentinel"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "left4dead2", "cfg")); err != nil {
		t.Skipf("directory symlink creation is unavailable: %v", err)
	}

	relative := "left4dead2/cfg/server.cfg"
	if _, _, _, err := ReadManagedFile(root, relative); err == nil {
		t.Fatal("read followed a symlinked managed parent")
	}
	if err := WriteManagedFile(root, relative, "outside-overwritten", ""); err == nil {
		t.Fatal("write followed a symlinked managed parent")
	}
	if err := DeleteManagedFile(root, relative); err == nil {
		t.Fatal("delete followed a symlinked managed parent")
	}
	if _, err := ListManagedFiles(root, "left4dead2/cfg"); err == nil {
		t.Fatal("list followed a symlinked managed parent")
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "outside-sentinel" {
		t.Fatalf("outside sentinel was changed: %q, %v", raw, err)
	}
}

func TestManagedFileOperationsRefuseSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.cfg")
	if err := os.WriteFile(outside, []byte("outside-sentinel"), 0o640); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "cfg")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "server.cfg")); err != nil {
		t.Skipf("file symlink creation is unavailable: %v", err)
	}
	if _, _, _, err := ReadManagedFile(root, "cfg/server.cfg"); err == nil {
		t.Fatal("read followed a managed-file symlink")
	}
	if err := DeleteManagedFile(root, "cfg/server.cfg"); err == nil {
		t.Fatal("delete accepted a managed-file symlink")
	}
	if err := WriteManagedFile(root, "cfg/server.cfg", "managed-replacement", ""); err != nil {
		t.Fatalf("atomic replacement of the symlink entry failed: %v", err)
	}
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "outside-sentinel" {
		t.Fatalf("outside file was changed: %q, %v", raw, err)
	}
	info, err := os.Lstat(filepath.Join(parent, "server.cfg"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("managed path was not replaced with a regular entry: %#v, %v", info, err)
	}
}

func TestManagedDirectoryListingUsesSafeHandle(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cfg", "server.cfg"), []byte("hostname safe\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	files, err := ListManagedFiles(root, "cfg")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "server.cfg" || files[0].IsDir || files[0].Size == 0 {
		t.Fatalf("unexpected managed directory listing: %#v", files)
	}
}

func TestOptionalAdminsSimpleShortcutTracksFileExistence(t *testing.T) {
	root := t.TempDir()
	if _, exists := optionalManagedFile(root, adminsSimpleRelativePath); exists {
		t.Fatal("missing admins_simple.ini was exposed as a shortcut")
	}
	fullPath := filepath.Join(root, filepath.FromSlash(adminsSimpleRelativePath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("\"STEAM_1:1:1\" \"99:z\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	item, exists := optionalManagedFile(root, adminsSimpleRelativePath)
	if !exists || item.Name != "admins_simple.ini" || item.Path != adminsSimpleRelativePath || item.IsDir {
		t.Fatalf("unexpected admins shortcut: exists=%v item=%#v", exists, item)
	}
}
