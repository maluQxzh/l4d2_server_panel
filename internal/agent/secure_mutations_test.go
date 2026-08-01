package agent

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecureWriterNoReplaceAndDigestFailureLeaveTargetUntouched(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "cfg", "server.cfg")
	if err := os.WriteFile(target, []byte("existing"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFileWithinNoFollow(root, "cfg/server.cfg", strings.NewReader("replacement"), secureWriteOptions{
		Mode: 0o640, ExpectedSize: int64(len("replacement")), Replace: false,
	}); err == nil {
		t.Fatal("exclusive secure commit replaced an existing file")
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "existing" {
		t.Fatalf("existing file changed after failed exclusive commit: %q, %v", raw, err)
	}
	if _, err := writeFileWithinNoFollow(root, "cfg/digest.cfg", strings.NewReader("payload"), secureWriteOptions{
		Mode: 0o640, ExpectedSize: 7, ExpectedSHA256: strings.Repeat("0", 64), Replace: true,
	}); err == nil {
		t.Fatal("secure commit accepted a mismatched SHA-256")
	}
	if _, err := os.Stat(filepath.Join(root, "cfg", "digest.cfg")); !os.IsNotExist(err) {
		t.Fatalf("digest-mismatched file was committed: %v", err)
	}
}

func TestGeneratedConfigWriterRejectsSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "left4dead2", "cfg")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.cfg")
	if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "l4d2panel.cfg")); err != nil {
		t.Skipf("file symlink creation is unavailable: %v", err)
	}
	if err := writeGeneratedServerConfig(root, []byte("new-secret")); err == nil {
		t.Fatal("generated config writer accepted a symlink target")
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "outside-secret" {
		t.Fatalf("outside generated-config target changed: %q, %v", raw, err)
	}
}

func TestGeneratedSecretConfigIsNotReadableThroughFileJobs(t *testing.T) {
	root := t.TempDir()
	if err := writeGeneratedServerConfig(root, []byte("rcon_password secret\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadManagedFile(root, generatedConfigRelative); err == nil {
		t.Fatal("public managed-file read exposed the generated secret configuration")
	}
	if raw, err := readRegularFileWithinNoFollow(root, generatedConfigRelative, maxManagedFileSize); err != nil || string(raw) != "rcon_password secret\n" {
		t.Fatalf("privileged generated-config reader failed: %q, %v", raw, err)
	}
}

func TestPackageInstallRejectsSymlinkedInstanceParent(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "package.zip")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	entry, err := writer.Create("cfg/server.cfg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("package-content")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	instance := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(instance, "left4dead2"), 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "server.cfg")
	if err := os.WriteFile(sentinel, []byte("outside-sentinel"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(instance, "left4dead2", "cfg")); err != nil {
		t.Skipf("directory symlink creation is unavailable: %v", err)
	}
	if _, err := InstallPackageArchive(instance, archivePath, t.TempDir(), false); err == nil {
		t.Fatal("package install followed a symlinked instance parent")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || !bytes.Equal(raw, []byte("outside-sentinel")) {
		t.Fatalf("outside package target changed: %q, %v", raw, err)
	}
}

func TestSecureMoveIsNoReplaceAndRejectsSymlinkedDestinationParent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "addons", "enabled"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "addons", "source.vpk"), []byte("source"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "addons", "enabled", "source.vpk"), []byte("existing"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := moveFileWithinNoFollow(root, "addons/source.vpk", "addons/enabled/source.vpk"); err == nil {
		t.Fatal("secure move overwrote an existing destination")
	}
	if raw, err := os.ReadFile(filepath.Join(root, "addons", "source.vpk")); err != nil || string(raw) != "source" {
		t.Fatalf("source changed after rejected move: %q, %v", raw, err)
	}

	outside := t.TempDir()
	outsideSentinel := filepath.Join(outside, "source.vpk")
	if err := os.WriteFile(outsideSentinel, []byte("outside"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "addons", "redirect")); err != nil {
		t.Skipf("directory symlink creation is unavailable: %v", err)
	}
	if err := moveFileWithinNoFollow(root, "addons/source.vpk", "addons/redirect/source.vpk"); err == nil {
		t.Fatal("secure move followed a symlinked destination parent")
	}
	if raw, err := os.ReadFile(outsideSentinel); err != nil || string(raw) != "outside" {
		t.Fatalf("outside move target changed: %q, %v", raw, err)
	}
}
