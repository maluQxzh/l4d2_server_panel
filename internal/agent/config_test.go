package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	raw := []byte("central_url: \"https://panel.example\"\nnode_name: test-node\ninstance_roots:\n  - ./servers\nsteamcmd_path: ./steamcmd.exe\ndata_dir: ./state\nsteamcmd_concurrency: 2\ninsecure_skip_verify: false\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeName != "test-node" || cfg.SteamCMDConcurrency != 2 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if !filepath.IsAbs(cfg.InstanceRoots[0]) || !filepath.IsAbs(cfg.DataDir) {
		t.Fatal("paths were not normalized to absolute paths")
	}
}

func TestConfigRejectsPlaintextRemoteControl(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	raw := []byte(`{"central_url":"http://panel.example","instance_roots":["./servers"],"data_dir":"./state"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected non-local plaintext URL to be rejected")
	}
}
