package agent

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestRestoreCheckpointExactRestoresOverwritesAndRemovesNewFiles(t *testing.T) {
	instancePath := t.TempDir()
	original := filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "configs", "core.cfg")
	if err := os.MkdirAll(filepath.Dir(original), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.zip")
	if _, err := CreateBackup("instance", instancePath, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	newNative := filepath.Join(instancePath, "left4dead2", "addons", "metamod", "bin", "server.so")
	if err := os.MkdirAll(filepath.Dir(newNative), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newNative, []byte("new native"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCheckpointExact(instancePath, checkpoint, filepath.Join(t.TempDir(), "restore-package-test"), "instance"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(original)
	if err != nil || string(raw) != "old" {
		t.Fatalf("original file was not restored: %q %v", raw, err)
	}
	if _, err := os.Stat(newNative); !os.IsNotExist(err) {
		t.Fatalf("new file survived exact rollback: %v", err)
	}
}

func TestRestoreCheckpointExactRejectsOtherInstanceBeforeWriting(t *testing.T) {
	instancePath := t.TempDir()
	config := filepath.Join(instancePath, "left4dead2", "cfg", "server.cfg")
	if err := os.MkdirAll(filepath.Dir(config), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.zip")
	if _, err := CreateBackup("other-instance", instancePath, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := restoreCheckpointExact(instancePath, checkpoint, filepath.Join(t.TempDir(), "restore-package-test"), "instance")
	if err == nil || !strings.Contains(err.Error(), "different instance") {
		t.Fatalf("expected instance mismatch, got %v", err)
	}
	raw, readErr := os.ReadFile(config)
	if readErr != nil || string(raw) != "live" {
		t.Fatalf("mismatched checkpoint changed live files: %q %v", raw, readErr)
	}
}

func TestThirdPartyServerExtensionRequiresNativeRiskConfirmation(t *testing.T) {
	root := t.TempDir()
	instancePath := filepath.Join(root, "instance")
	if err := os.MkdirAll(instancePath, 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestMetamodLoader(t, instancePath)
	paths, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "instance", RootIndex: 0, RelativePath: "instance"}); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{paths: paths, registry: registry, logger: log.New(io.Discard, "", 0)}
	_, err = executor.installMod(context.Background(), domain.Job{
		ID: "job", InstanceID: "instance", Payload: map[string]any{"kind": "sourcemod", "source": "https://example.com/untrusted.zip"},
	}, func(int, string) {})
	if err == nil || !strings.Contains(err.Error(), "acknowledge_native_risk") {
		t.Fatalf("expected high-risk confirmation failure, got %v", err)
	}
}

func TestSourceModRequiresInstalledMetamodBeforeResolvingPackage(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "instance"), 0o750); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "instance", RootIndex: 0, RelativePath: "instance"}); err != nil {
		t.Fatal(err)
	}
	resolverCalled := false
	executor := &Executor{
		paths: paths, registry: registry, logger: log.New(io.Discard, "", 0),
		officialPackageResolver: func(context.Context, string) (OfficialPackage, error) {
			resolverCalled = true
			return OfficialPackage{}, errors.New("resolver should not be called")
		},
	}
	_, err = executor.installMod(context.Background(), domain.Job{
		ID: "job", InstanceID: "instance", Payload: map[string]any{"kind": "sourcemod", "rcon_password": "secret"},
	}, func(int, string) {})
	if err == nil || !strings.Contains(err.Error(), "requires an installed Metamod:Source") {
		t.Fatalf("expected Metamod dependency failure, got %v", err)
	}
	if resolverCalled {
		t.Fatal("SourceMod package was resolved before checking the Metamod dependency")
	}
}

func TestMetamodDependencyRequiresTrustedLoaderFiles(t *testing.T) {
	instancePath := t.TempDir()
	vdfPath := filepath.Join(instancePath, "left4dead2", "addons", "metamod.vdf")
	if err := os.MkdirAll(filepath.Dir(vdfPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vdfPath, []byte(`"Plugin" { "file" "../addons/metamod/bin/server" }`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := requireMetamodInstalled(instancePath); err == nil {
		t.Fatal("Metamod dependency accepted a loader descriptor without its native binary")
	}
	binaryPath := filepath.Join(instancePath, "left4dead2", "addons", "metamod", "bin", testMetamodBinaryName())
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("metamod"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := requireMetamodInstalled(instancePath); err != nil {
		t.Fatalf("valid Metamod loader files were rejected: %v", err)
	}
}

func writeTestMetamodLoader(t *testing.T, instancePath string) {
	t.Helper()
	vdfPath := filepath.Join(instancePath, "left4dead2", "addons", "metamod.vdf")
	if err := os.MkdirAll(filepath.Dir(vdfPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vdfPath, []byte(`"Plugin" { "file" "../addons/metamod/bin/server" }`), 0o640); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(instancePath, "left4dead2", "addons", "metamod", "bin", testMetamodBinaryName())
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("metamod"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func testMetamodBinaryName() string {
	if runtime.GOOS == "windows" {
		return "server.dll"
	}
	return "server.so"
}

func TestTrustedPackageArchiveCanInstallNativeFiles(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "metamod.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("addons/metamod/bin/server.dll")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("native")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	instancePath := t.TempDir()
	if _, err := InstallPackageArchive(instancePath, archivePath, t.TempDir(), false); err == nil {
		t.Fatal("expected an untrusted native archive to be rejected")
	}
	files, err := InstallPackageArchive(instancePath, archivePath, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "left4dead2/addons/metamod/bin/server.dll" {
		t.Fatalf("unexpected installed files: %#v", files)
	}
}

func TestExtensionVerificationCommands(t *testing.T) {
	if extensionVerificationCommand("metamod") != "meta version" || extensionVerificationCommand("sourcemod") != "sm version" {
		t.Fatal("extension verification commands changed")
	}
	if extensionVerificationCommand("other") != "" {
		t.Fatal("unknown extensions must not have a verification command")
	}
}
