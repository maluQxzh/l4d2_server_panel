package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bodgit/sevenzip"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestPluginBackupCreatesReadableSevenZipLayouts(t *testing.T) {
	for _, layout := range []string{pluginBackupLayoutPreserve, pluginBackupLayoutCompact} {
		t.Run(layout, func(t *testing.T) {
			executor, _, instancePath := uploadedPackageExecutor(t)
			files := map[string]string{
				"left4dead2/addons/sourcemod/plugins/sample.smx":        "compiled plugin",
				"left4dead2/addons/sourcemod/gamedata/sample.games.txt": "game data",
				"left4dead2/cfg/sourcemod/sample.cfg":                   "generated config",
			}
			managed := make([]domain.PluginBackupFile, 0, len(files))
			for relative, content := range files {
				path := filepath.Join(instancePath, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(content))
				managed = append(managed, domain.PluginBackupFile{Path: relative, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), Entrypoint: strings.HasSuffix(relative, ".smx")})
			}
			result, err := executor.pluginBackup(context.Background(), domain.Job{
				ID: "plugin-backup-" + layout, InstanceID: "instance", Payload: map[string]any{
					"layout": layout, "plugin_packages": []domain.PluginBackupPackage{{
						ID: "package-one", Name: "Sample Plugin", Kind: "plugin", Enabled: true, Files: managed,
					}},
				},
			}, func(int, string) {})
			if err != nil {
				t.Fatal(err)
			}
			archivePath := result["path"].(string)
			reader, err := sevenzip.OpenReader(archivePath)
			if err != nil {
				t.Fatalf("open generated 7z: %v", err)
			}
			defer reader.Close()
			actual := make(map[string]string, len(reader.File))
			for _, archived := range reader.File {
				entry, err := archived.Open()
				if err != nil {
					t.Fatal(err)
				}
				raw, readErr := io.ReadAll(entry)
				closeErr := entry.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("read %s: %v / %v", archived.Name, readErr, closeErr)
				}
				actual[archived.Name] = string(raw)
			}
			for relative, content := range files {
				expected := "Sample Plugin/" + relative
				if layout == pluginBackupLayoutCompact {
					expected = "Sample Plugin/" + pluginBackupEntryPath("", relative, layout)[1:]
				}
				if actual[expected] != content {
					keys := make([]string, 0, len(actual))
					for key := range actual {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					t.Fatalf("entry %q mismatch; entries=%v", expected, keys)
				}
			}
			if result["kind"] != "plugin_"+layout || result["files"] != len(files) {
				t.Fatalf("unexpected backup result: %#v", result)
			}
		})
	}
}

func TestPluginBackupRejectsChangedManagedFile(t *testing.T) {
	executor, dataDir, instancePath := uploadedPackageExecutor(t)
	relative := "left4dead2/addons/sourcemod/plugins/sample.smx"
	path := filepath.Join(instancePath, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := executor.pluginBackup(context.Background(), domain.Job{ID: "plugin-backup-invalid", InstanceID: "instance", Payload: map[string]any{
		"layout": pluginBackupLayoutPreserve, "plugin_packages": []domain.PluginBackupPackage{{ID: "package-one", Name: "Sample", Kind: "plugin", Enabled: true,
			Files: []domain.PluginBackupFile{{Path: relative, SHA256: strings.Repeat("0", 64), Size: 7, Entrypoint: true}}}},
	}}, func(int, string) {})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 changed") {
		t.Fatalf("expected changed file rejection, got %v", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(dataDir, "backups", "instance", "*.tmp"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("temporary archive was not cleaned: %v, %v", matches, globErr)
	}
}

func TestPluginBackupFoldersSanitizeWindowsDeviceNames(t *testing.T) {
	packages := []domain.PluginBackupPackage{{
		ID: "device-package", Name: "CON", Kind: "plugin",
		Files: []domain.PluginBackupFile{{Path: "left4dead2/addons/sourcemod/plugins/con.smx"}},
	}}
	folders, _, err := pluginBackupFolders(packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || strings.EqualFold(folders[0], "CON") {
		t.Fatalf("Windows device name was not replaced: %#v", folders)
	}
}

func TestPluginBackupReadsDisabledSmartEntrypointFromManagedState(t *testing.T) {
	executor, _, _ := uploadedPackageExecutor(t)
	const packageID = "11111111-1111-4111-8111-111111111111"
	const target = "left4dead2/addons/sourcemod/plugins/disabled-smart.smx"
	content := []byte("disabled compiled plugin")
	sum := sha256.Sum256(content)
	stateRoot, err := executor.smartPluginStateRoot("instance", packageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateRoot, "disabled"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "disabled", "entry.smx"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	state := smartPluginState{Version: 1, PackageID: packageID, InstanceID: "instance", Name: "Disabled Smart", Enabled: false,
		Files: []smartPluginStateFile{{ID: "entry", TargetPath: target, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), Entrypoint: true}}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := executor.pluginBackup(context.Background(), domain.Job{ID: "disabled-smart-backup", InstanceID: "instance", Payload: map[string]any{
		"layout": pluginBackupLayoutCompact, "plugin_packages": []domain.PluginBackupPackage{{ID: packageID, Name: "Disabled Smart", Kind: "smart", Enabled: false,
			Files: []domain.PluginBackupFile{{Path: target, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), Entrypoint: true}}}},
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := sevenzip.OpenReader(result["path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) != 1 || reader.File[0].Name != "Disabled Smart/plugins/disabled-smart.smx" {
		t.Fatalf("unexpected disabled smart archive entries: %#v", reader.File)
	}
	entry, err := reader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	archived, readErr := io.ReadAll(entry)
	closeErr := entry.Close()
	if readErr != nil || closeErr != nil || string(archived) != string(content) {
		t.Fatalf("unexpected archived entry: %q, %v, %v", archived, readErr, closeErr)
	}
}
