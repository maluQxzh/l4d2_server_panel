package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestSmartPluginInstallToggleDeleteRestoresTakeover(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	existingConfig := filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "sample.cfg")
	if err := os.MkdirAll(filepath.Dir(existingConfig), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existingConfig, []byte("old config"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive := smartTestZIP(t, map[string]string{
		"Sample/left4dead2/addons/sourcemod/plugins/sample.smx": "smx-v1",
		"Sample/left4dead2/cfg/sourcemod/sample.cfg":            "new config",
	})
	encoded := base64.StdEncoding.EncodeToString(archive)
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-smart", InstanceID: "instance", Payload: map[string]any{"name": "sample.zip", "content_base64": encoded},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folders, ok := inspection["folders"].([]any)
	if !ok || len(folders) != 1 {
		t.Fatalf("unexpected inspection: %#v", inspection)
	}
	folder := folders[0].(map[string]any)
	files := folder["files"].([]any)
	takeovers := make([]any, 0, 1)
	for _, raw := range files {
		file := raw.(map[string]any)
		if file["existing"] == true {
			takeovers = append(takeovers, file["id"])
		}
	}
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-smart", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "sample.zip", "content_base64": encoded,
			"inspection_fingerprint": inspection["fingerprint"],
			"smart_packages":         []any{map[string]any{"folder_id": folder["id"], "takeovers": takeovers}},
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	packages := result["packages"].([]map[string]any)
	packageID := packages[0]["package_id"].(string)
	assertFileContent(t, existingConfig, "new config")
	smx := filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "plugins", "sample.smx")
	assertFileContent(t, smx, "smx-v1")
	state, _, err := executor.readSmartPluginState("instance", packageID)
	if err != nil {
		t.Fatal(err)
	}
	var configSHA string
	for _, file := range state.Files {
		if file.TargetPath == "left4dead2/cfg/sourcemod/sample.cfg" {
			configSHA = file.SHA256
		}
	}
	editResult, err := executor.file(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{
		"operation": "smart_plugin_edit", "package_id": packageID, "relative_path": "left4dead2/cfg/sourcemod/sample.cfg",
		"content": "operator config", "expected_sha256": configSHA,
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, existingConfig, "operator config")
	readResult, err := executor.file(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{
		"operation": "smart_plugin_read", "package_id": packageID, "relative_path": "left4dead2/cfg/sourcemod/sample.cfg",
		"expected_sha256": editResult["sha256"],
	}}, func(int, string) {})
	if err != nil || readResult["content_base64"] != base64.StdEncoding.EncodeToString([]byte("operator config")) {
		t.Fatalf("smart file read failed: result=%#v err=%v", readResult, err)
	}
	updatedState, _, err := executor.readSmartPluginState("instance", packageID)
	if err != nil || editResult["aggregate_sha256"] != updatedState.AggregateSHA || updatedState.AggregateSHA == state.AggregateSHA {
		t.Fatalf("smart file edit did not update the manifest: result=%#v state=%#v err=%v", editResult, updatedState, err)
	}
	archiveV2 := smartTestZIP(t, map[string]string{
		"Sample/left4dead2/addons/sourcemod/plugins/sample.smx": "smx-v2",
		"Sample/left4dead2/cfg/sourcemod/sample.cfg":            "updated config",
	})
	encodedV2 := base64.StdEncoding.EncodeToString(archiveV2)
	registered := map[string]string{
		"left4dead2/addons/sourcemod/plugins/sample.smx": packageID,
		"left4dead2/cfg/sourcemod/sample.cfg":            packageID,
	}
	inspectionV2, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-smart-v2", InstanceID: "instance", Payload: map[string]any{"name": "sample-v2.zip", "content_base64": encodedV2, "registered_paths": registered},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folderV2 := inspectionV2["folders"].([]any)[0].(map[string]any)
	if _, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-smart-v2", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "sample-v2.zip", "content_base64": encodedV2,
			"inspection_fingerprint": inspectionV2["fingerprint"], "registered_paths": registered,
			"smart_packages": []any{map[string]any{"folder_id": folderV2["id"], "package_id": packageID}},
		},
	}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, existingConfig, "updated config")
	assertFileContent(t, smx, "smx-v2")

	if _, err := executor.togglePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{"kind": "smart", "package_id": packageID, "enabled": false}}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(smx); !os.IsNotExist(err) {
		t.Fatalf("SMX entrypoint still exists after disable: %v", err)
	}
	assertFileContent(t, existingConfig, "updated config")
	if _, err := executor.togglePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{"kind": "smart", "package_id": packageID, "enabled": true}}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, smx, "smx-v2")

	deleted, err := executor.deletePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{"kind": "smart", "package_id": packageID}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if deleted["package_id"] != packageID {
		t.Fatalf("unexpected delete result: %#v", deleted)
	}
	if _, err := os.Stat(smx); !os.IsNotExist(err) {
		t.Fatalf("SMX still exists after deletion: %v", err)
	}
	assertFileContent(t, existingConfig, "old config")
}

func smartTestZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != expected {
		t.Fatalf("%s = %q, want %q", path, raw, expected)
	}
}

func TestSmartPluginShowCorpus(t *testing.T) {
	root := os.Getenv("PLUGIN_SHOW_CORPUS")
	if root == "" {
		t.Skip("set PLUGIN_SHOW_CORPUS to validate the local categorized plugin corpus")
	}
	var files []extractedMapFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(filepath.Dir(root), path)
		if err != nil {
			return err
		}
		files = append(files, extractedMapFile{Path: path, Relative: filepath.ToSlash(relative)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildSmartPluginPlan(root, "pluginshow.zip", "archive-sha", "zip", 0, files, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Folders) != 53 || plan.FileCount != 476 {
		t.Fatalf("folders/files = %d/%d, want 53/476", len(plan.Folders), plan.FileCount)
	}
}

func TestMapSmartPluginLooseFiles(t *testing.T) {
	t.Parallel()
	siblings := map[string]bool{"c12.nut": true, "l4dtoolz.vdf": true}
	tests := []struct {
		name, target string
		ambiguous    bool
		native       bool
	}{
		{"admin.smx", "left4dead2/addons/sourcemod/plugins/admin.smx", false, false},
		{"admin.sp", "left4dead2/addons/sourcemod/scripting/admin.sp", false, false},
		{"shared.inc", "left4dead2/addons/sourcemod/scripting/include/shared.inc", false, false},
		{"admin.phrases.txt", "left4dead2/addons/sourcemod/translations/admin.phrases.txt", false, false},
		{"sdktools.games.txt", "left4dead2/addons/sourcemod/gamedata/sdktools.games.txt", false, false},
		{"c12.txt", "left4dead2/modes/c12.txt", false, false},
		{"settings.cfg", "left4dead2/cfg/sourcemod/settings.cfg", true, false},
		{"helper.txt", "left4dead2/addons/sourcemod/gamedata/helper.txt", true, false},
		{"sample.ext.dll", "left4dead2/addons/sourcemod/extensions/sample.ext.dll", false, true},
		{"l4dtoolz.dll", "left4dead2/addons/l4dtoolz.dll", false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped, err := mapSmartPluginFile(test.name, siblings)
			if err != nil {
				t.Fatal(err)
			}
			if got := firstNonEmpty(mapped.TargetPath, mapped.SuggestedPath); got != test.target {
				t.Fatalf("target = %q, want %q", got, test.target)
			}
			if mapped.Ambiguous != test.ambiguous || mapped.Native != test.native {
				t.Fatalf("ambiguous/native = %v/%v, want %v/%v", mapped.Ambiguous, mapped.Native, test.ambiguous, test.native)
			}
		})
	}
}

func TestBuildSmartPluginPlanSplitsCollectionWrapper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inputs := []string{
		"pluginshow/Admin/left4dead2/addons/sourcemod/plugins/admin.smx",
		"pluginshow/Vote/left4dead2/cfg/vote/menu.cfg",
	}
	files := make([]extractedMapFile, 0, len(inputs))
	for _, relative := range inputs {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(relative), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, extractedMapFile{Path: path, Relative: relative})
	}
	plan, err := buildSmartPluginPlan(root, "pluginshow.zip", "archive-sha", "zip", 123, files, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Folders) != 2 || plan.FileCount != 2 {
		t.Fatalf("folders/files = %d/%d, want 2/2", len(plan.Folders), plan.FileCount)
	}
	if plan.Folders[0].Name != "Admin" || plan.Folders[1].Name != "Vote" {
		t.Fatalf("unexpected folders: %#v", plan.Folders)
	}
	if plan.Folders[0].Files[0].TargetPath != "left4dead2/addons/sourcemod/plugins/admin.smx" {
		t.Fatalf("unexpected explicit target: %q", plan.Folders[0].Files[0].TargetPath)
	}
}

func TestGroupSmartPluginFilesStripsSingleCollectionWrapper(t *testing.T) {
	t.Parallel()
	files := []extractedMapFile{{Relative: "bundle/Only/left4dead2/addons/sourcemod/plugins/only.smx"}}
	groups, err := groupSmartPluginFiles("bundle.zip", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].name != "Only" || groups[0].prefix != "bundle/Only" {
		t.Fatalf("unexpected groups: %#v", groups)
	}
}

func TestValidateSmartPluginTargetRejectsEscapeAndWrongRoot(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"../left4dead2/addons/a.smx", "left4dead2/logs/a.smx", "left4dead2/addons/a.cfg", "left4dead2/addons/a.smx:stream"} {
		if err := validateSmartPluginTarget(target, ".smx"); err == nil {
			t.Fatalf("expected %q to be rejected", target)
		}
	}
}
