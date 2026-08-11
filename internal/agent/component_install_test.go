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

func TestSourceModArchivePreservesAdminsSimpleAndRejectsOtherPackageTargets(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "sourcemod.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, content := range map[string]string{
		"addons/metamod/sourcemod.vdf":               "SourceMod loader",
		"addons/sourcemod/configs/admins_simple.ini": "archive admins",
		"addons/sourcemod/bin/sourcemod.logic.dll":   "core binary",
		"cfg/sourcemod/sourcemod.cfg":                "component config",
	} {
		entry, createErr := writer.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write([]byte(content)); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	instancePath := t.TempDir()
	adminsPath := filepath.Join(instancePath, filepath.FromSlash(sourceModAdminsSimplePath))
	if err := os.MkdirAll(filepath.Dir(adminsPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adminsPath, []byte("operator admins"), 0o640); err != nil {
		t.Fatal(err)
	}
	files, preserved, err := installPackageArchive(instancePath, archivePath, t.TempDir(), packageArchiveInstallOptions{
		AllowNative: true, PreserveExisting: map[string]bool{sourceModAdminsSimplePath: true},
		ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget("sourcemod", value) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 || len(preserved) != 1 || preserved[0] != sourceModAdminsSimplePath {
		t.Fatalf("unexpected install inventory: files=%#v preserved=%#v", files, preserved)
	}
	assertFileContent(t, adminsPath, "operator admins")
	assertFileContent(t, filepath.Join(instancePath, "left4dead2", "addons", "metamod", "sourcemod.vdf"), "SourceMod loader")
	assertFileContent(t, filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "bin", "sourcemod.logic.dll"), "core binary")
	assertFileContent(t, filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "sourcemod.cfg"), "component config")

	freshInstance := t.TempDir()
	_, freshPreserved, err := installPackageArchive(freshInstance, archivePath, t.TempDir(), packageArchiveInstallOptions{
		AllowNative: true, PreserveExisting: map[string]bool{sourceModAdminsSimplePath: true},
		ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget("sourcemod", value) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(freshPreserved) != 0 {
		t.Fatalf("missing admins_simple.ini was unexpectedly treated as preserved: %#v", freshPreserved)
	}
	assertFileContent(t, filepath.Join(freshInstance, filepath.FromSlash(sourceModAdminsSimplePath)), "archive admins")

	otherInstance := t.TempDir()
	_, _, err = installPackageArchive(otherInstance, archivePath, t.TempDir(), packageArchiveInstallOptions{
		AllowNative: true, PackageID: "component", RegisteredPaths: map[string]string{
			"left4dead2/addons/sourcemod/bin/sourcemod.logic.dll": "other-package",
		}, PreserveExisting: map[string]bool{sourceModAdminsSimplePath: true},
		ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget("sourcemod", value) },
	})
	if err == nil || !strings.Contains(err.Error(), "managed by another package") {
		t.Fatalf("cross-package overwrite was not rejected: %v", err)
	}
}

func TestMetamodArchiveAllowsOfficialRootLoaders(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "metamod.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, name := range []string{"addons/metamod.vdf", "addons/metamod_x64.vdf", "addons/metamod/bin/server.dll"} {
		entry, createErr := writer.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write([]byte(name)); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	instancePath := t.TempDir()
	_, _, err = installPackageArchive(instancePath, archivePath, t.TempDir(), packageArchiveInstallOptions{
		AllowNative:    true,
		ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget("metamod", value) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"left4dead2/addons/metamod.vdf", "left4dead2/addons/metamod_x64.vdf", "left4dead2/addons/metamod/bin/server.dll"} {
		assertFileContent(t, filepath.Join(instancePath, filepath.FromSlash(relative)), strings.TrimPrefix(relative, "left4dead2/"))
	}
}

func TestServerExtensionArchiveRejectsOutsideComponentDirectoryBeforeWrite(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "invalid-sourcemod.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, name := range []string{"addons/sourcemod/bin/sourcemod.logic.dll", "cfg/server.cfg"} {
		entry, createErr := writer.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write([]byte(name)); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	instancePath := t.TempDir()
	_, _, err = installPackageArchive(instancePath, archivePath, t.TempDir(), packageArchiveInstallOptions{
		AllowNative:    true,
		ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget("sourcemod", value) },
	})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("outside SourceMod target was not rejected: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "bin", "sourcemod.logic.dll")); !os.IsNotExist(statErr) {
		t.Fatalf("archive preflight wrote files before rejecting an invalid target: %v", statErr)
	}
}

func TestServerExtensionManagedTargetsUseFixedComponentDirectories(t *testing.T) {
	for _, candidate := range []struct{ kind, path string }{
		{"metamod", "left4dead2/addons/metamod_x64.vdf"},
		{"metamod", "left4dead2/addons/metamod/bin/server.dll"},
		{"sourcemod", "left4dead2/addons/metamod/sourcemod.vdf"},
		{"sourcemod", "left4dead2/cfg/sourcemod/sourcemod.cfg"},
	} {
		if err := validateServerExtensionManagedTarget(candidate.kind, candidate.path); err != nil {
			t.Errorf("official component target was rejected: %#v: %v", candidate, err)
		}
	}
	for _, candidate := range []struct{ kind, path string }{
		{"metamod", "left4dead2/cfg/server.cfg"},
		{"sourcemod", "left4dead2/addons/custom-component/file.txt"},
		{"sourcemod", sourceModAdminsSimplePath},
	} {
		if err := validateServerExtensionManagedTarget(candidate.kind, candidate.path); err == nil {
			t.Errorf("unsafe or protected package target was accepted: %#v", candidate)
		}
	}
}

func TestOfficialWindowsComponentArchiveFixtures(t *testing.T) {
	fixtureDir := strings.TrimSpace(os.Getenv("L4D2PANEL_COMPONENT_FIXTURE_DIR"))
	if fixtureDir == "" {
		t.Skip("set L4D2PANEL_COMPONENT_FIXTURE_DIR to validate downloaded official component archives")
	}
	for _, fixture := range []struct{ kind, name string }{
		{"metamod", "mmsource-1.12.0-git1225-windows.zip"},
		{"sourcemod", "sourcemod-1.12.0-git7246-windows.zip"},
	} {
		t.Run(fixture.kind, func(t *testing.T) {
			archivePath := filepath.Join(fixtureDir, fixture.name)
			archive, err := zip.OpenReader(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			files := 0
			for _, entry := range archive.File {
				if entry.FileInfo().IsDir() {
					continue
				}
				target, err := packageDestination(entry.Name, true)
				if err != nil {
					t.Fatalf("official entry %q has no safe package destination: %v", entry.Name, err)
				}
				if err := validateServerExtensionInstallTarget(fixture.kind, target); err != nil {
					t.Fatalf("official entry %q maps to rejected target %q: %v", entry.Name, target, err)
				}
				files++
			}
			if files == 0 {
				t.Fatal("official component fixture contains no files")
			}

			instancePath := t.TempDir()
			options := packageArchiveInstallOptions{
				AllowNative:    true,
				ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget(fixture.kind, value) },
			}
			if fixture.kind == "sourcemod" {
				options.PreserveExisting = map[string]bool{sourceModAdminsSimplePath: true}
				adminsPath := filepath.Join(instancePath, filepath.FromSlash(sourceModAdminsSimplePath))
				if err := os.MkdirAll(filepath.Dir(adminsPath), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(adminsPath, []byte("fixture admins"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			installed, preserved, err := installPackageArchive(instancePath, archivePath, t.TempDir(), options)
			if err != nil {
				t.Fatalf("install official %s fixture: %v", fixture.kind, err)
			}
			if len(installed) != files {
				t.Fatalf("installed %d files, archive contains %d", len(installed), files)
			}
			if fixture.kind == "sourcemod" {
				if len(preserved) != 1 || preserved[0] != sourceModAdminsSimplePath {
					t.Fatalf("unexpected protected SourceMod files: %#v", preserved)
				}
				assertFileContent(t, filepath.Join(instancePath, filepath.FromSlash(sourceModAdminsSimplePath)), "fixture admins")
			}
		})
	}
}

func TestDeleteSourceModPackageKeepsAdminsAndUnmanagedPlugin(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	record, _ := executor.registry.Get("instance")
	record.Config = domain.InstanceConfig{Mode: "coop", StartMap: "c1m1_hotel", MaxPlayers: 8}
	if err := executor.registry.Put(record); err != nil {
		t.Fatal(err)
	}
	coreRelative := "left4dead2/addons/sourcemod/bin/sourcemod.logic.dll"
	corePath := filepath.Join(instancePath, filepath.FromSlash(coreRelative))
	adminsPath := filepath.Join(instancePath, filepath.FromSlash(sourceModAdminsSimplePath))
	unmanagedPath := filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "plugins", "custom.smx")
	for path, content := range map[string]string{corePath: "core", adminsPath: "operator admins", unmanagedPath: "custom plugin"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	coreSHA, coreSize, err := hashRegularFileWithinNoFollow(instancePath, coreRelative, maxPackageExpanded)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.deletePackage(context.Background(), domain.Job{ID: "delete-component", InstanceID: "instance", Payload: map[string]any{
		"package_id": "component", "kind": "sourcemod", "expected_sha256": strings.Repeat("a", 64),
		"managed_files": []any{map[string]any{"path": coreRelative, "sha256": coreSHA, "size": coreSize}},
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["package_id"] != "component" {
		t.Fatalf("unexpected deletion result: %#v", result)
	}
	if _, err := os.Stat(corePath); !os.IsNotExist(err) {
		t.Fatalf("managed SourceMod file still exists: %v", err)
	}
	assertFileContent(t, adminsPath, "operator admins")
	assertFileContent(t, unmanagedPath, "custom plugin")

	if _, err := executor.deletePackage(context.Background(), domain.Job{ID: "delete-protected", InstanceID: "instance", Payload: map[string]any{
		"package_id": "component", "kind": "sourcemod", "expected_sha256": strings.Repeat("a", 64),
		"managed_files": []any{map[string]any{"path": sourceModAdminsSimplePath, "sha256": strings.Repeat("b", 64), "size": int64(15)}},
	}}, func(int, string) {}); err == nil {
		t.Fatal("protected admins_simple.ini was accepted as a deletion target")
	}
	assertFileContent(t, adminsPath, "operator admins")
}

func TestExtensionVerificationCommands(t *testing.T) {
	if extensionVerificationCommand("metamod") != "meta version" || extensionVerificationCommand("sourcemod") != "sm version" {
		t.Fatal("extension verification commands changed")
	}
	if extensionVerificationCommand("other") != "" {
		t.Fatal("unknown extensions must not have a verification command")
	}
}
