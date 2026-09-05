package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestPendingCancellationSurvivesAgentCapabilityDowngrade(t *testing.T) {
	store, instance := projectionRecoveryStore(t)
	ctx := context.Background()
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "install_mod", map[string]any{"kind": "smart"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatched(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CancelJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// An unsupported new job must remain queued, while the old cancellation
	// needs only the base protocol's job_cancel message and its job ID.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status)
		VALUES('unsupported-job','node','game.delete','queued')`); err != nil {
		t.Fatal(err)
	}
	hub := NewAgentHub(store, NewEventBroker(store), slog.New(slog.NewTextHandler(io.Discard, nil)), []byte("0123456789abcdef0123456789abcdef"))
	defer hub.Close()
	node, err := store.GetNode(ctx, "node")
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
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var cancellation protocol.Envelope
	if err := connection.ReadJSON(&cancellation); err != nil {
		t.Fatalf("capability gating stranded a pending cancellation: %v", err)
	}
	if cancellation.Type != protocol.TypeJobCancel || cancellation.JobID != job.ID {
		t.Fatalf("unsupported work was dispatched instead of cancellation: %#v", cancellation)
	}
	update, err := protocol.NewEnvelope(protocol.TypeJobUpdate, "node", job.ID, protocol.JobUpdate{
		Status: domain.JobCanceled, Progress: 100, Message: "canceled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(update); err != nil {
		t.Fatal(err)
	}
	for {
		var reply protocol.Envelope
		if err := connection.ReadJSON(&reply); err != nil {
			t.Fatalf("cancellation was not durably acknowledged: %v", err)
		}
		if reply.Type == protocol.TypeJobCancel && reply.JobID == job.ID {
			continue // The durable outbox may repeat until the terminal commit.
		}
		if reply.Type != protocol.TypeJobAck || reply.JobID != job.ID {
			t.Fatalf("unexpected reply: %#v", reply)
		}
		break
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, "", ""); err != nil {
		t.Fatalf("acknowledged cancellation still blocks deletion: %v", err)
	}
}
