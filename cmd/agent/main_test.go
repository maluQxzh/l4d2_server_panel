package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
)

func TestDirectConfigPathUsesExecutableDirectory(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "release", "l4d2panel-agent-linux-amd64")
	want := filepath.Join(filepath.Dir(executable), "agent.yaml")
	if got := directConfigPath(executable); got != want {
		t.Fatalf("directConfigPath() = %q, want %q", got, want)
	}
}

func TestPrepareDirectConfigCreatesFirstStartConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	cfg, err := prepareDirectConfig(path, "http://localhost:19090", "one-time-token", "wsl-node")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CentralURL != "http://localhost:19090" || cfg.EnrollmentToken != "one-time-token" || cfg.NodeName != "wsl-node" {
		t.Fatalf("unexpected direct config: %#v", cfg)
	}
	if !filepath.IsAbs(cfg.DataDir) || len(cfg.InstanceRoots) != 1 || !filepath.IsAbs(cfg.InstanceRoots[0]) {
		t.Fatalf("portable defaults were not normalized: %#v", cfg)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config mode = %o, want no group/other permissions", info.Mode().Perm())
	}
}

func TestPrepareDirectConfigRequiresBothBootstrapValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	_, err := prepareDirectConfig(path, "http://localhost:19090", "", "")
	if err == nil || !strings.Contains(err.Error(), "central_url and enrollment_token") {
		t.Fatalf("prepareDirectConfig() error = %v", err)
	}
}

func TestPrepareDirectConfigDoesNotRebindEnrolledNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	cfg := &agent.Config{
		CentralURL: "https://panel.example.com",
		NodeID:     "node-1",
		Credential: "credential-1",
	}
	if err := agent.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := prepareDirectConfig(path, "https://panel.example.com/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NodeID != "node-1" || loaded.Credential != "credential-1" {
		t.Fatalf("enrolled identity changed: %#v", loaded)
	}
	if _, err := prepareDirectConfig(path, "https://other.example.com", "", ""); err == nil {
		t.Fatal("expected a different control plane to be rejected")
	}
	if _, err := prepareDirectConfig(path, "", "new-token", ""); err == nil {
		t.Fatal("expected an enrollment token on an enrolled node to be rejected")
	}
}

func TestDirectFirstEnrollmentRemovesOneTimeToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/enroll" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Token != "one-time-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"node_id":"node-1","credential":"credential-1"}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "agent.yaml")
	cfg, err := prepareDirectConfig(path, server.URL, "one-time-token", "wsl-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Enroll(context.Background(), path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "one-time-token") || strings.Contains(string(raw), "enrollment_token") {
		t.Fatalf("saved config retained the one-time token: %s", raw)
	}
	if !strings.Contains(string(raw), "credential-1") || !strings.Contains(string(raw), "node-1") {
		t.Fatalf("saved config omitted enrolled identity: %s", raw)
	}
}
