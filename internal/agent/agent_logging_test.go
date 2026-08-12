package agent

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestRunLogsConnectionLifecycleWithoutSecrets(t *testing.T) {
	const credential = "node-credential-secret"
	connected := make(chan struct{})
	var connectedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/v1/connect" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello protocol.Envelope
		if err := conn.ReadJSON(&hello); err != nil || hello.Type != protocol.TypeHello {
			return
		}
		connectedOnce.Do(func() { close(connected) })
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	dataDir := t.TempDir()
	config := &Config{
		CentralURL:        server.URL,
		Credential:        credential,
		NodeID:            "node-1",
		NodeName:          "test-node",
		DataDir:           dataDir,
		InstanceRoots:     []string{filepath.Join(dataDir, "instances")},
		HeartbeatInterval: "100ms",
		MetricsInterval:   "100ms",
	}
	var output bytes.Buffer
	agent, err := New(filepath.Join(dataDir, "agent.yaml"), config, log.New(&output, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	waitForSignal(t, connected, "Agent connection")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Agent run failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Agent shutdown")
	}

	logs := output.String()
	for _, want := range []string{"starting agent", "connecting to control plane", "control connection established", "agent stopped"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("lifecycle log %q missing from %q", want, logs)
		}
	}
	for _, secret := range []string{credential, server.URL} {
		if strings.Contains(logs, secret) {
			t.Fatalf("lifecycle logs exposed sensitive connection data %q", secret)
		}
	}
}

func TestUpdateJournalLogsPersistenceFailureWithoutJobContent(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "job-log-test", Type: "file", Payload: map[string]any{"content": "payload-secret"}}
	if existing, err := journal.Begin(job); err != nil || existing != nil {
		t.Fatalf("claim test job: existing=%v err=%v", existing, err)
	}
	journal.path = t.TempDir()
	var output bytes.Buffer
	agent := &Agent{journal: journal, logger: log.New(&output, "", 0)}
	agent.updateJournal(job.ID, domain.JobFailed, 1, "message-secret", map[string]any{"result": "result-secret"})

	logs := output.String()
	for _, want := range []string{"persist job journal update", job.ID, string(domain.JobFailed)} {
		if !strings.Contains(logs, want) {
			t.Fatalf("journal persistence log %q missing from %q", want, logs)
		}
	}
	for _, secret := range []string{"payload-secret", "message-secret", "result-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("journal persistence log exposed job content %q", secret)
		}
	}
}
