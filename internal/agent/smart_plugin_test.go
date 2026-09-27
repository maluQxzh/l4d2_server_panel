package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestSmartPluginInstallToggleDeleteFullyOwnsTakeover(t *testing.T) {
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
	state, stateRoot, err := executor.readSmartPluginState("instance", packageID)
	if err != nil {
		t.Fatal(err)
	}
	var configSHA string
	for _, file := range state.Files {
		if file.TargetPath == "left4dead2/cfg/sourcemod/sample.cfg" {
			configSHA = file.SHA256
			if file.Baseline || file.BaselineSHA256 != "" {
				t.Fatalf("takeover persisted a restore baseline: %#v", file)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "baseline")); !os.IsNotExist(err) {
		t.Fatalf("takeover baseline directory was persisted: %v", err)
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

	deleteState, _, err := executor.readSmartPluginState("instance", packageID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := executor.deletePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{"kind": "smart", "package_id": packageID, "expected_sha256": deleteState.AggregateSHA}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if deleted["package_id"] != packageID {
		t.Fatalf("unexpected delete result: %#v", deleted)
	}
	if _, err := os.Stat(smx); !os.IsNotExist(err) {
		t.Fatalf("SMX still exists after deletion: %v", err)
	}
	if _, err := os.Stat(existingConfig); !os.IsNotExist(err) {
		t.Fatalf("taken-over config still exists after deletion: %v", err)
	}
}

func TestSmartPluginDeleteIgnoresLegacyTakeoverBaseline(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	const (
		packageID = "11111111-1111-4111-8111-111111111111"
		target    = "left4dead2/cfg/sourcemod/legacy.cfg"
		fileID    = "legacy-file"
	)
	targetPath := filepath.Join(instancePath, filepath.FromSlash(target))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("managed config"), 0o640); err != nil {
		t.Fatal(err)
	}
	sha, size, err := hashRegularFileWithinNoFollow(instancePath, target, maxSmartPluginEntry)
	if err != nil {
		t.Fatal(err)
	}
	state := smartPluginState{
		Version: 1, PackageID: packageID, InstanceID: "instance", Name: "Legacy", Enabled: true,
		ArchiveSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", UpdatedAt: time.Now().UTC(),
		Files: []smartPluginStateFile{{
			ID: fileID, TargetPath: target, SHA256: sha, Size: size,
			Baseline: true, BaselineSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}},
	}
	state.AggregateSHA = smartPluginAggregateSHA(state.Files)
	if old, err := executor.writeSmartPluginState(state); err != nil || old != "" {
		t.Fatalf("write legacy smart plugin state: old=%q err=%v", old, err)
	}
	stateRoot, err := executor.smartPluginStateRoot("instance", packageID)
	if err != nil {
		t.Fatal(err)
	}
	baselineDir := filepath.Join(stateRoot, "baseline")
	if err := os.MkdirAll(baselineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baselineDir, fileID+".cfg"), []byte("original config"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := executor.deletePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{
		"kind": "smart", "package_id": packageID, "expected_sha256": state.AggregateSHA,
	}}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("legacy taken-over file was restored instead of deleted: %v", err)
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("legacy smart plugin state still exists after deletion: %v", err)
	}
}

func TestSmartPluginReconcilesGeneratedSameNameConfigAndDeletesIt(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	archive := smartTestZIP(t, map[string]string{
		"Sample/left4dead2/addons/sourcemod/plugins/sample.smx": "smx",
	})
	encoded := base64.StdEncoding.EncodeToString(archive)
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-generated-config", InstanceID: "instance", Payload: map[string]any{"name": "sample.zip", "content_base64": encoded},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folder := inspection["folders"].([]any)[0].(map[string]any)
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-generated-config", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "sample.zip", "content_base64": encoded,
			"inspection_fingerprint": inspection["fingerprint"],
			"smart_packages":         []any{map[string]any{"folder_id": folder["id"]}},
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	packageID := result["packages"].([]map[string]any)[0]["package_id"].(string)
	generated := filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "sample.cfg")
	if err := os.MkdirAll(filepath.Dir(generated), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("// generated by SourceMod\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	legacyResult := executor.attachSmartPluginGeneratedConfigs(context.Background(), "instance", "stop", nil, false, map[string]any{"status": "stopped"})
	if _, claimed := legacyResult["smart_plugin_config_updates"]; claimed {
		t.Fatalf("lifecycle job without a Control ownership snapshot claimed a config: %#v", legacyResult)
	}
	registeredElsewhere := map[string]string{"left4dead2/cfg/sourcemod/sample.cfg": "another-package"}
	if updates, err := executor.reconcileSmartPluginGeneratedConfigs("instance", registeredElsewhere, ""); err != nil || len(updates) != 0 {
		t.Fatalf("registered config was claimed by a smart plugin: updates=%#v err=%v", updates, err)
	}
	state, _, err := executor.readSmartPluginState("instance", packageID)
	if err != nil || len(state.Files) != 1 {
		t.Fatalf("registered config changed local smart state: state=%#v err=%v", state, err)
	}

	updates, err := executor.reconcileSmartPluginGeneratedConfigs("instance", nil, "")
	if err != nil || len(updates) != 1 || updates[0]["package_id"] != packageID || updates[0]["file_count"] != 2 {
		t.Fatalf("unexpected generated config reconciliation: updates=%#v err=%v", updates, err)
	}
	state, _, err = executor.readSmartPluginState("instance", packageID)
	if err != nil || len(state.Files) != 2 {
		t.Fatalf("generated config was not added to smart state: state=%#v err=%v", state, err)
	}
	var recorded bool
	for _, file := range state.Files {
		if file.TargetPath == "left4dead2/cfg/sourcemod/sample.cfg" && !file.Entrypoint && file.Size == int64(len("// generated by SourceMod\n")) {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("generated config missing from smart state: %#v", state.Files)
	}
	if repeated, err := executor.reconcileSmartPluginGeneratedConfigs("instance", nil, ""); err != nil || len(repeated) != 0 {
		t.Fatalf("generated config reconciliation was not idempotent: updates=%#v err=%v", repeated, err)
	}
	archiveV2 := smartTestZIP(t, map[string]string{
		"Sample/left4dead2/addons/sourcemod/plugins/sample.smx": "smx-v2",
	})
	encodedV2 := base64.StdEncoding.EncodeToString(archiveV2)
	registered := map[string]string{
		"left4dead2/addons/sourcemod/plugins/sample.smx": packageID,
		"left4dead2/cfg/sourcemod/sample.cfg":            packageID,
	}
	inspectionV2, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-generated-config-v2", InstanceID: "instance", Payload: map[string]any{
			"name": "sample-v2.zip", "content_base64": encodedV2, "registered_paths": registered,
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folderV2 := inspectionV2["folders"].([]any)[0].(map[string]any)
	if _, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-generated-config-v2", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "sample-v2.zip", "content_base64": encodedV2,
			"inspection_fingerprint": inspectionV2["fingerprint"], "registered_paths": registered,
			"smart_packages": []any{map[string]any{"folder_id": folderV2["id"], "package_id": packageID}},
		},
	}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, generated, "// generated by SourceMod\n")
	assertFileContent(t, filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "plugins", "sample.smx"), "smx-v2")
	state, _, err = executor.readSmartPluginState("instance", packageID)
	if err != nil || len(state.Files) != 2 || !state.Files[1].Generated {
		t.Fatalf("plugin update did not preserve generated config: state=%#v err=%v", state, err)
	}
	if _, err := executor.deletePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{
		"kind": "smart", "package_id": packageID, "expected_sha256": state.AggregateSHA,
	}}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("generated config still exists after smart plugin deletion: %v", err)
	}
}

func TestSmartPluginGeneratedConfigSkipsAmbiguousSameNameEntrypoints(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	archive := smartTestZIP(t, map[string]string{
		"Bundle/One/left4dead2/addons/sourcemod/plugins/one/sample.smx": "one",
		"Bundle/Two/left4dead2/addons/sourcemod/plugins/two/sample.smx": "two",
	})
	encoded := base64.StdEncoding.EncodeToString(archive)
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-ambiguous-config", InstanceID: "instance", Payload: map[string]any{"name": "bundle.zip", "content_base64": encoded},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folders := inspection["folders"].([]any)
	if len(folders) != 2 {
		t.Fatalf("unexpected smart folders: %#v", inspection)
	}
	selections := make([]any, 0, len(folders))
	for _, raw := range folders {
		folder := raw.(map[string]any)
		selections = append(selections, map[string]any{"folder_id": folder["id"]})
	}
	if _, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-ambiguous-config", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "bundle.zip", "content_base64": encoded,
			"inspection_fingerprint": inspection["fingerprint"], "smart_packages": selections,
		},
	}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "sample.cfg")
	if err := os.MkdirAll(filepath.Dir(generated), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("ambiguous"), 0o640); err != nil {
		t.Fatal(err)
	}
	updates, err := executor.reconcileSmartPluginGeneratedConfigs("instance", nil, "")
	if err != nil || len(updates) != 0 {
		t.Fatalf("ambiguous generated config was claimed: updates=%#v err=%v", updates, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if polled := executor.pollSmartPluginGeneratedConfigs(ctx, "instance", map[string]string{}, time.Hour, smartPluginGeneratedConfigMaxPollAttempts); len(polled) != 0 || ctx.Err() != nil {
		t.Fatalf("ambiguous SMX ownership unnecessarily entered startup polling: updates=%#v err=%v", polled, ctx.Err())
	}
}

func TestSmartPluginGeneratedConfigStartupPollingCapturesDelayedFile(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	archive := smartTestZIP(t, map[string]string{
		"Delayed/left4dead2/addons/sourcemod/plugins/delayed.smx": "smx",
	})
	encoded := base64.StdEncoding.EncodeToString(archive)
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-delayed-config", InstanceID: "instance", Payload: map[string]any{"name": "delayed.zip", "content_base64": encoded},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folder := inspection["folders"].([]any)[0].(map[string]any)
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-delayed-config", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "delayed.zip", "content_base64": encoded,
			"inspection_fingerprint": inspection["fingerprint"],
			"smart_packages":         []any{map[string]any{"folder_id": folder["id"]}},
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	packageID := result["packages"].([]map[string]any)[0]["package_id"].(string)
	generated := filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "delayed.cfg")
	writeDone := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(generated), 0o750); err != nil {
			writeDone <- err
			return
		}
		writeDone <- os.WriteFile(generated, []byte("delayed generated config"), 0o640)
	}()
	updates := executor.pollSmartPluginGeneratedConfigs(context.Background(), "instance", map[string]string{}, 10*time.Millisecond, 50)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0]["package_id"] != packageID || updates[0]["file_count"] != 2 {
		t.Fatalf("startup polling missed delayed generated config: %#v", updates)
	}
	state, _, err := executor.readSmartPluginState("instance", packageID)
	if err != nil || len(state.Files) != 2 || !state.Files[1].Generated {
		t.Fatalf("delayed config was not persisted in smart plugin state: state=%#v err=%v", state, err)
	}
}

func TestSmartPluginGeneratedConfigStartupPollBounds(t *testing.T) {
	if smartPluginGeneratedConfigPollInterval != 3*time.Second || smartPluginGeneratedConfigMaxPollAttempts != 5 {
		t.Fatalf("unexpected startup polling bounds: interval=%v attempts=%d", smartPluginGeneratedConfigPollInterval, smartPluginGeneratedConfigMaxPollAttempts)
	}
}

func TestSmartPluginDeleteReconcilesLateGeneratedConfig(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	archive := smartTestZIP(t, map[string]string{
		"Late/left4dead2/addons/sourcemod/plugins/late.smx": "smx",
	})
	encoded := base64.StdEncoding.EncodeToString(archive)
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-late-config", InstanceID: "instance", Payload: map[string]any{"name": "late.zip", "content_base64": encoded},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folder := inspection["folders"].([]any)[0].(map[string]any)
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-late-config", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "late.zip", "content_base64": encoded,
			"inspection_fingerprint": inspection["fingerprint"],
			"smart_packages":         []any{map[string]any{"folder_id": folder["id"]}},
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	packageID := result["packages"].([]map[string]any)[0]["package_id"].(string)
	state, _, err := executor.readSmartPluginState("instance", packageID)
	if err != nil || len(state.Files) != 1 {
		t.Fatalf("unexpected initial smart state: state=%#v err=%v", state, err)
	}
	generated := filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "late.cfg")
	if err := os.MkdirAll(filepath.Dir(generated), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generated, []byte("late generated config"), 0o640); err != nil {
		t.Fatal(err)
	}
	deleted, err := executor.deletePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{
		"kind": "smart", "package_id": packageID, "expected_sha256": state.AggregateSHA, "registered_paths": map[string]string{},
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if deleted["sha256"] != state.AggregateSHA || deleted["files"] != 2 {
		t.Fatalf("late generated config was not included in deletion: %#v", deleted)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("late generated config still exists after deletion: %v", err)
	}
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

func TestSmartPluginArchiveDecodesLegacyGBKZIPPaths(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	entryName := "中文插件/left4dead2/addons/sourcemod/plugins/sample.smx"
	legacyName, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(entryName))
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: string(legacyName), Method: zip.Store, NonUTF8: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("compiled plugin")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(archive.Bytes())
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-gbk-smart", InstanceID: "instance", Payload: map[string]any{
			"name": "legacy.zip", "content_base64": encoded, "path_encoding": domain.MapPathEncodingAuto,
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if inspection["version"] != float64(smartPluginPlanVersion) || inspection["path_encoding"] != domain.MapPathEncodingAuto {
		t.Fatalf("unexpected inspection metadata: %#v", inspection)
	}
	folder := inspection["folders"].([]any)[0].(map[string]any)
	file := folder["files"].([]any)[0].(map[string]any)
	if folder["name"] != "中文插件" || file["source_path"] != entryName || strings.ContainsRune(file["source_path"].(string), '�') {
		t.Fatalf("legacy GBK path was not decoded losslessly: folder=%#v file=%#v", folder["name"], file["source_path"])
	}
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "install-gbk-smart", InstanceID: "instance", Payload: map[string]any{
			"kind": "smart", "name": "legacy.zip", "content_base64": encoded, "path_encoding": domain.MapPathEncodingAuto,
			"inspection_fingerprint": inspection["fingerprint"],
			"smart_packages":         []any{map[string]any{"folder_id": folder["id"]}},
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	packages := result["packages"].([]map[string]any)
	if len(packages) != 1 || packages[0]["name"] != "中文插件" {
		t.Fatalf("decoded plugin name was not retained: %#v", packages)
	}
	assertFileContent(t, filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "plugins", "sample.smx"), "compiled plugin")

	if _, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{
		ID: "inspect-gbk-as-utf8", InstanceID: "instance", Payload: map[string]any{
			"name": "legacy.zip", "content_base64": encoded, "path_encoding": domain.MapPathEncodingUTF8,
		},
	}, func(int, string) {}); err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("explicit UTF-8 accepted a legacy GBK path: %v", err)
	}
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
	plan, err := buildSmartPluginPlan(root, "pluginshow.zip", "archive-sha", domain.MapPathEncodingAuto, "zip", 0, files, t.TempDir())
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
	plan, err := buildSmartPluginPlan(root, "pluginshow.zip", "archive-sha", domain.MapPathEncodingAuto, "zip", 123, files, t.TempDir())
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

func TestSmartPluginDeleteAllowsExplicitCleanupAndPreservesChangedConfig(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	const packageID = "22222222-2222-4222-8222-222222222222"
	configPath := "left4dead2/cfg/sourcemod/hostname.txt"
	programPath := "left4dead2/addons/sourcemod/plugins/example.smx"
	configFile := filepath.Join(instancePath, filepath.FromSlash(configPath))
	programFile := filepath.Join(instancePath, filepath.FromSlash(programPath))
	if err := os.MkdirAll(filepath.Dir(configFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(programFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, []byte("changed by operator"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(programFile, []byte("changed program"), 0o640); err != nil {
		t.Fatal(err)
	}
	configOriginal := sha256.Sum256([]byte("original config"))
	configSHA, configSize := hex.EncodeToString(configOriginal[:]), int64(len("original config"))
	programSHA := sha256.Sum256([]byte("original program"))
	state := smartPluginState{Version: 1, PackageID: packageID, InstanceID: "instance", Name: "Example", Enabled: true,
		ArchiveSHA: strings.Repeat("a", 64), UpdatedAt: time.Now().UTC(), Files: []smartPluginStateFile{
			{ID: "config", TargetPath: configPath, SHA256: configSHA, Size: configSize},
			{ID: "program", TargetPath: programPath, SHA256: hex.EncodeToString(programSHA[:]), Size: int64(len("original program")), Entrypoint: true},
		}}
	state.AggregateSHA = smartPluginAggregateSHA(state.Files)
	if _, err := executor.writeSmartPluginState(state); err != nil {
		t.Fatal(err)
	}
	result, err := executor.deletePackage(context.Background(), domain.Job{InstanceID: "instance", Payload: map[string]any{
		"kind": "smart", "package_id": packageID, "expected_sha256": state.AggregateSHA,
		"allow_changed_files": true, "preserve_changed_config": true,
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configFile); err != nil {
		t.Fatalf("changed config was not preserved: %v", err)
	}
	if _, err := os.Stat(programFile); !os.IsNotExist(err) {
		t.Fatalf("changed program was not deleted: %v", err)
	}
	preserved, ok := result["preserved_config_files"].([]string)
	if !ok || len(preserved) != 1 || preserved[0] != configPath {
		t.Fatalf("unexpected preserved result: %#v", result)
	}
}

func TestSmartPluginInstallCanAdoptExistingFileWithoutOverwriting(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	target := filepath.Join(instancePath, "left4dead2", "cfg", "sourcemod", "hostname.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("operator hostname"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive := smartTestZIP(t, map[string]string{"Host/left4dead2/cfg/sourcemod/hostname.txt": "archive hostname"})
	encoded := base64.StdEncoding.EncodeToString(archive)
	inspection, err := executor.inspectSmartPluginArchive(context.Background(), domain.Job{ID: "inspect-preserve", InstanceID: "instance", Payload: map[string]any{"name": "host.zip", "content_base64": encoded}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	folder := inspection["folders"].([]any)[0].(map[string]any)
	files := folder["files"].([]any)
	fileID := files[0].(map[string]any)["id"].(string)
	result, err := executor.installMod(context.Background(), domain.Job{ID: "install-preserve", InstanceID: "instance", Payload: map[string]any{
		"kind": "smart", "name": "host.zip", "content_base64": encoded,
		"inspection_fingerprint": inspection["fingerprint"],
		"smart_packages":         []any{map[string]any{"folder_id": folder["id"], "preserve_existing": []any{fileID}}},
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target, "operator hostname")
	pkg := result["packages"].([]map[string]any)[0]
	managed := pkg["files"].([]map[string]any)
	if len(managed) != 1 || managed[0]["sha256"] != inspection["folders"].([]any)[0].(map[string]any)["files"].([]any)[0].(map[string]any)["existing_sha256"] {
		t.Fatalf("preserved file was not projected with its existing digest: %#v", result)
	}
}
