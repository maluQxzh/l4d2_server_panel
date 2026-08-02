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

func TestConfigPlaintextControlAddressPolicy(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		allowed bool
	}{
		{name: "localhost", url: "http://localhost:19090", allowed: true},
		{name: "localhost trailing dot", url: "ws://LOCALHOST.:19090", allowed: true},
		{name: "IPv4 loopback", url: "http://127.0.0.1:19090", allowed: true},
		{name: "IPv4 private 10", url: "http://10.20.30.40:19090", allowed: true},
		{name: "IPv4 private 172", url: "http://172.31.255.254:19090", allowed: true},
		{name: "IPv4 private 192", url: "http://192.168.1.20:19090", allowed: true},
		{name: "IPv4 link local", url: "http://169.254.10.20:19090", allowed: true},
		{name: "IPv6 loopback", url: "http://[::1]:19090", allowed: true},
		{name: "IPv6 private", url: "http://[fd12:3456::10]:19090", allowed: true},
		{name: "IPv6 link local", url: "http://[fe80::10%25eth0]:19090", allowed: true},
		{name: "public hostname", url: "http://panel.example", allowed: false},
		{name: "LAN hostname is not pinned", url: "http://panel.lan:19090", allowed: false},
		{name: "public IPv4", url: "http://8.8.8.8:19090", allowed: false},
		{name: "outside RFC1918", url: "http://172.32.0.1:19090", allowed: false},
		{name: "public IPv6", url: "http://[2001:4860:4860::8888]:19090", allowed: false},
		{name: "public HTTPS", url: "https://panel.example", allowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.json")
			raw := []byte(`{"central_url":"` + tt.url + `","instance_roots":["./servers"],"data_dir":"./state"}`)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if tt.allowed && err != nil {
				t.Fatalf("expected URL to be allowed: %v", err)
			}
			if !tt.allowed && err == nil {
				t.Fatal("expected plaintext URL to be rejected")
			}
		})
	}
}
