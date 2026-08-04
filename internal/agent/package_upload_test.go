package agent

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func uploadedPackageExecutor(t *testing.T) (*Executor, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	root := t.TempDir()
	instancePath := filepath.Join(root, "instance")
	if err := os.MkdirAll(instancePath, 0o750); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "instance", RootIndex: 0, RelativePath: "instance"}); err != nil {
		t.Fatal(err)
	}
	return &Executor{config: &Config{DataDir: dataDir}, paths: paths, registry: registry, logger: log.New(io.Discard, "", 0)}, dataDir, instancePath
}

func TestInstallUploadedVPKPreservesNameAndRemovesStagingFile(t *testing.T) {
	executor, dataDir, instancePath := uploadedPackageExecutor(t)
	addons := filepath.Join(instancePath, "left4dead2", "addons")
	if err := os.MkdirAll(addons, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(addons, "unrelated-invalid.vpk"), []byte("existing file is not inspected"), 0o640); err != nil {
		t.Fatal(err)
	}
	raw := validTestVPK([]byte("uploaded"), 2)
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "upload-vpk", InstanceID: "instance", Payload: map[string]any{
			"kind": "vpk_url", "name": "campaign.vpk", "content_base64": base64.StdEncoding.EncodeToString(raw),
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["kind"] != "vpk" || result["path"] != "left4dead2/addons/campaign.vpk" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(instancePath, "left4dead2", "addons", "campaign.vpk")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging upload was not removed: entries=%v err=%v", entries, err)
	}
}

func TestInstallUploadedPluginAndRejectUnconfirmedNative(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	result, err := executor.installMod(context.Background(), domain.Job{
		ID: "upload-smx", InstanceID: "instance", Payload: map[string]any{
			"kind": "plugin", "name": "welcome.smx", "content_base64": base64.StdEncoding.EncodeToString([]byte("plugin")),
		},
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["path"] != "left4dead2/addons/sourcemod/plugins/welcome.smx" {
		t.Fatalf("unexpected plugin result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "plugins", "welcome.smx")); err != nil {
		t.Fatal(err)
	}

	_, err = executor.installMod(context.Background(), domain.Job{
		ID: "upload-native", InstanceID: "instance", Payload: map[string]any{
			"kind": "plugin", "name": "unsafe.dll", "content_base64": base64.StdEncoding.EncodeToString([]byte("native")),
		},
	}, func(int, string) {})
	if err == nil {
		t.Fatal("expected an unconfirmed native extension to be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(instancePath, "left4dead2", "addons", "sourcemod", "extensions", "unsafe.dll")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected native extension was written: %v", statErr)
	}
}
