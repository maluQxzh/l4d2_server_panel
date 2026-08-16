package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestTerminalOutcomeFingerprintIsStableAndSensitive(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	first := protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100, Message: "done",
		Result: map[string]any{"build_id": "123", "nested": map[string]any{"ok": true}}}
	second := protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100, Message: "done",
		Result: map[string]any{"nested": map[string]any{"ok": true}, "build_id": "123", jobProjectionPendingField: true}}
	firstFingerprint, err := terminalJobUpdateFingerprint(key, "job-1", first)
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint, err := terminalJobUpdateFingerprint(key, "job-1", second)
	if err != nil || !terminalFingerprintsEqual(firstFingerprint, secondFingerprint) {
		t.Fatalf("equivalent outcomes had different fingerprints: %q %q err=%v", firstFingerprint, secondFingerprint, err)
	}
	second.Result["build_id"] = "changed"
	changed, err := terminalJobUpdateFingerprint(key, "job-1", second)
	if err != nil {
		t.Fatal(err)
	}
	if terminalFingerprintsEqual(firstFingerprint, changed) {
		t.Fatal("changed terminal result retained the original fingerprint")
	}
}

func TestTerminalReplayEquivalentSupportsFingerprintAndLegacySecret(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	update := protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100, Message: "done",
		Result: map[string]any{"response": "status output"}}
	fingerprint, err := terminalJobUpdateFingerprint(key, "job-2", update)
	if err != nil {
		t.Fatal(err)
	}
	fingerprinted := domain.Job{ID: "job-2", Type: "rcon", Status: update.Status, Progress: update.Progress,
		Message: update.Message, Result: map[string]any{terminalOutcomeFingerprintField: fingerprint}}
	if equivalent, err := terminalReplayEquivalent(key, fingerprinted, update, fingerprint); err != nil || !equivalent {
		t.Fatalf("fingerprinted replay rejected: equivalent=%v err=%v", equivalent, err)
	}

	ciphertext, err := encryptSecret(key, "status output")
	if err != nil {
		t.Fatal(err)
	}
	legacy := domain.Job{ID: "job-2", Type: "rcon", Status: update.Status, Progress: update.Progress,
		Message: update.Message, Result: map[string]any{"response": encryptedValuePlaceholder, "response_ciphertext": ciphertext}}
	if equivalent, err := terminalReplayEquivalent(key, legacy, update, fingerprint); err != nil || !equivalent {
		t.Fatalf("equivalent legacy encrypted replay rejected: equivalent=%v err=%v", equivalent, err)
	}
	changed := update
	changed.Result = map[string]any{"response": "different output"}
	changedFingerprint, err := terminalJobUpdateFingerprint(key, "job-2", changed)
	if err != nil {
		t.Fatal(err)
	}
	if equivalent, err := terminalReplayEquivalent(key, legacy, changed, changedFingerprint); err != nil || equivalent {
		t.Fatalf("changed legacy outcome accepted: equivalent=%v err=%v", equivalent, err)
	}
}

func TestPersistedTerminalOutcomeIsImmutable(t *testing.T) {
	t.Parallel()
	current := domain.Job{Status: domain.JobSucceeded, Progress: 100, Message: "done",
		Result: map[string]any{"build_id": "123", "count": float64(2), jobProjectionPendingField: true}}
	if !persistedTerminalOutcomeEquivalent(current, domain.JobSucceeded, 100, "done", map[string]any{"count": 2, "build_id": "123"}) {
		t.Fatal("identical structural replay was rejected")
	}
	if persistedTerminalOutcomeEquivalent(current, domain.JobSucceeded, 100, "done", map[string]any{"build_id": "456"}) {
		t.Fatal("changed structural replay was accepted")
	}
	current.Result[terminalOutcomeFingerprintField] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	matching := map[string]any{terminalOutcomeFingerprintField: current.Result[terminalOutcomeFingerprintField]}
	if !persistedTerminalOutcomeEquivalent(current, domain.JobSucceeded, 100, "done", matching) {
		t.Fatal("matching fingerprinted replay was rejected")
	}
	matching[terminalOutcomeFingerprintField] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if persistedTerminalOutcomeEquivalent(current, domain.JobSucceeded, 100, "done", matching) {
		t.Fatal("mismatched fingerprinted replay was accepted")
	}
}

func TestDeferredProjectionClassification(t *testing.T) {
	t.Parallel()
	projectedTypes := []string{"update", "install_mod", "rollback_package", "backup.upload.chunk", "ban_player", "delete", "purge_quarantine"}
	for _, status := range []domain.JobStatus{domain.JobSucceeded, domain.JobFailed} {
		for _, jobType := range projectedTypes {
			if !requiresDeferredJobProjection(jobType, status) {
				t.Fatalf("%s %s did not require deferred projection", jobType, status)
			}
		}
		if requiresDeferredJobProjection("restore", status) {
			t.Fatalf("restore %s incorrectly required deferred projection", status)
		}
		if requiresDeferredJobProjection("inspect_plugin_archive", status) {
			t.Fatalf("archive inspection %s incorrectly required deferred projection", status)
		}
	}
	if requiresDeferredJobProjection("update", domain.JobCanceled) {
		t.Fatal("atomically canceled job required deferred projection")
	}
}

func TestTerminalPluginArchiveInspectionScrubsUploadWithoutProjection(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "plugin-inspection-terminal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	nodeID, jobID := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'Node','online');
		INSERT INTO jobs(id,node_id,type,status,progress,payload,result,created_at)
		VALUES(?2,?1,'inspect_plugin_archive','running',20,
		'{"source":"[encrypted]","source_ciphertext":"sealed-source","content_base64":"[encrypted]","content_base64_ciphertext":"sealed-upload"}',
		'{}',?3)`, nodeID, jobID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	updated, err := store.UpdateJob(ctx, jobID, domain.JobFailed, 20, "unsupported plugin file", map[string]any{"kind": "smart"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != domain.JobFailed || jobProjectionPending(updated) {
		t.Fatalf("archive inspection retained a projection barrier: %#v", updated)
	}
	if updated.Payload["content_base64_ciphertext"] != nil || updated.Payload["source_ciphertext"] != nil {
		t.Fatalf("archive inspection retained terminal upload secrets: %#v", updated.Payload)
	}
}

func TestTerminalOutcomeConflictIsNonfatalAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "terminal-conflict.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	nodeID, jobID := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'Node','online');
		INSERT INTO jobs(id,node_id,type,status,progress,message,payload,result,created_at,finished_at)
		VALUES(?2,?1,'discover_instances','succeeded',100,'control done','{}','{"value":"stored"}',?3,?3)`,
		nodeID, jobID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	hub := NewAgentHub(store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), []byte("0123456789abcdef0123456789abcdef"))
	hub.conns[nodeID] = &agentConnection{terminalConflicts: make(map[string]string)}
	envelope, err := protocol.NewEnvelope(protocol.TypeJobUpdate, nodeID, jobID, protocol.JobUpdate{
		Status: domain.JobFailed, Progress: 100, Message: "agent failed", Result: map[string]any{"value": "reported"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := hub.handleEnvelope(ctx, domain.Node{ID: nodeID, Name: "Node"}, envelope)
		if !errors.Is(err, errTerminalOutcomeConflict) {
			t.Fatalf("mismatched terminal replay error = %v", err)
		}
		if agentMessageFailureClosesConnection(envelope.Type, err) {
			t.Fatal("terminal outcome conflict was classified as a connection-fatal error")
		}
	}
	var audits, notifications int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action='job.terminal_replay_rejected' AND resource_id=?1`, jobID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE type='job.terminal_outcome_conflict' AND resource=?1`, jobID).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if audits != 1 || notifications != 1 {
		t.Fatalf("conflict records: audits=%d notifications=%d, want 1/1", audits, notifications)
	}
	conflicts, err := store.ListTerminalOutcomeConflicts(ctx, true, 100)
	if err != nil || len(conflicts) != 1 || conflicts[0].Occurrences != 2 {
		t.Fatalf("durable conflicts = %#v err=%v", conflicts, err)
	}
	requestFingerprint := "f" + strings.Repeat("0", 63)
	resolved, replay, err := store.ResolveTerminalOutcomeConflict(ctx, jobID, conflicts[0].ReportedFingerprint,
		owner.ID, "resolve-once", requestFingerprint)
	if err != nil || replay || resolved.Resolution != "keep_control" || resolved.ResolvedAt == nil {
		t.Fatalf("resolve conflict = %#v replay=%v err=%v", resolved, replay, err)
	}
	if _, replay, err := store.ResolveTerminalOutcomeConflict(ctx, jobID, conflicts[0].ReportedFingerprint,
		owner.ID, "resolve-once", requestFingerprint); err != nil || !replay {
		t.Fatalf("idempotent conflict resolution: replay=%v err=%v", replay, err)
	}
	if _, _, err := store.ResolveTerminalOutcomeConflict(ctx, jobID, conflicts[0].ReportedFingerprint,
		owner.ID, "different-key", requestFingerprint); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting resolution replay = %v", err)
	}
	if active, err := store.ListTerminalOutcomeConflicts(ctx, true, 100); err != nil || len(active) != 0 {
		t.Fatalf("resolved conflict remained active: %#v err=%v", active, err)
	}
	persisted, err := store.GetJob(ctx, jobID)
	if err != nil || persisted.Status != domain.JobSucceeded || persisted.Message != "control done" || persisted.Result["value"] != "stored" {
		t.Fatalf("conflict changed immutable job: %#v err=%v", persisted, err)
	}
}

func TestOnlyRetryableJobUpdateFailureClosesAgentConnection(t *testing.T) {
	if agentMessageFailureClosesConnection(protocol.TypeJobUpdate, errTerminalOutcomeConflict) {
		t.Fatal("terminal semantic conflict closed the Agent connection")
	}
	if agentMessageFailureClosesConnection(protocol.TypeJobUpdate, errors.New("permanent protocol rejection")) {
		t.Fatal("permanent job update rejection closed the Agent connection")
	}
	if !agentMessageFailureClosesConnection(protocol.TypeJobUpdate, retryableAgentMessage(errors.New("persist projection"))) {
		t.Fatal("retryable job update failure did not close the Agent connection")
	}
	if agentMessageFailureClosesConnection(protocol.TypeHeartbeat, retryableAgentMessage(errors.New("persist heartbeat"))) {
		t.Fatal("non-job message failure closed the Agent connection")
	}
}

func TestPermanentTerminalReplayRejectionKeepsAgentConnectionAlive(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "permanent-rejection.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	nodeID, acceptedJobID := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'Node','online');
		INSERT INTO jobs(id,node_id,type,status,progress,payload,result,created_at)
		VALUES(?2,?1,'discover_instances','running',50,'{}','{}',?3)`, nodeID, acceptedJobID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	events := NewEventBroker(store)
	hub := NewAgentHub(store, events, slog.New(slog.NewTextHandler(io.Discard, nil)), []byte("0123456789abcdef0123456789abcdef"))
	defer hub.Close()
	node, err := store.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(node, "127.0.0.1", w, r)
	}))
	defer server.Close()

	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	stale, err := protocol.NewEnvelope(protocol.TypeJobUpdate, nodeID, "unknown-stale-job", protocol.JobUpdate{
		Status: domain.JobFailed, Progress: 100, Message: "stale terminal replay",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(stale); err != nil {
		t.Fatal(err)
	}
	accepted, err := protocol.NewEnvelope(protocol.TypeJobUpdate, nodeID, acceptedJobID, protocol.JobUpdate{
		Status: domain.JobCanceled, Progress: 100, Message: "canceled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(accepted); err != nil {
		t.Fatalf("connection closed after permanent stale replay rejection: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	var acknowledgement protocol.Envelope
	if err := connection.ReadJSON(&acknowledgement); err != nil {
		t.Fatalf("connection did not process the next valid terminal outcome: %v", err)
	}
	if acknowledgement.Type != protocol.TypeJobAck || acknowledgement.JobID != acceptedJobID {
		t.Fatalf("acknowledgement = %#v", acknowledgement)
	}
}
