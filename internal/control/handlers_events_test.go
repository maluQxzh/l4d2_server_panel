package control

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func browserEventTestServer() *Server {
	return &Server{
		events:                   NewEventBroker(nil),
		eventAuthRefreshInterval: time.Hour,
		eventCatchupInterval:     time.Hour,
		revalidateEventPrincipal: func(_ context.Context, principal Principal) (Principal, error) { return principal, nil },
		eventsAfter:              func(context.Context, int64, int) ([]Event, error) { return nil, nil },
	}
}

func dialBrowserEventTestServer(t *testing.T, server *Server) *websocket.Conn {
	t.Helper()
	connection, cleanup := dialEventHandler(t, server, Principal{
		User: domain.User{Role: domain.RoleOwner}, ExpiresAt: time.Now().Add(time.Hour),
	})
	t.Cleanup(cleanup)
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestBrowserEventsRejectClientData(t *testing.T) {
	for _, test := range []struct {
		name        string
		messageType int
		size        int
		closeCode   int
	}{
		{"empty_text", websocket.TextMessage, 0, websocket.ClosePolicyViolation},
		{"text", websocket.TextMessage, 16, websocket.ClosePolicyViolation},
		{"binary", websocket.BinaryMessage, 16, websocket.ClosePolicyViolation},
		{"text_at_limit", websocket.TextMessage, 1024, websocket.ClosePolicyViolation},
		{"binary_at_limit", websocket.BinaryMessage, 1024, websocket.ClosePolicyViolation},
		{"oversized_text", websocket.TextMessage, 1025, websocket.CloseMessageTooBig},
		{"oversized_binary", websocket.BinaryMessage, 1025, websocket.CloseMessageTooBig},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := dialBrowserEventTestServer(t, browserEventTestServer())
			if err := connection.WriteMessage(test.messageType, []byte(strings.Repeat("x", test.size))); err != nil {
				t.Fatal(err)
			}
			_, _, err := connection.ReadMessage()
			if !websocket.IsCloseError(err, test.closeCode) {
				// Closing without consuming a forbidden payload can produce a TCP
				// reset on Windows before the close frame reaches the client. The
				// header-only tests below verify the exact protocol close codes.
				var networkError *net.OpError
				if errors.As(err, &networkError) && networkError.Op == "read" && !networkError.Timeout() {
					return
				}
				t.Fatalf("client data returned %v, want close code %d", err, test.closeCode)
			}
		})
	}
}

func TestBrowserEventsRejectDataWithoutReadingPayload(t *testing.T) {
	for _, test := range []struct {
		name      string
		header    []byte
		closeCode int
	}{
		// Masked frames advertise payload bytes that are deliberately never sent.
		// Fragmented messages also omit the final continuation frame entirely.
		{"text_header_only", []byte{0x81, 0x81, 1, 2, 3, 4}, websocket.ClosePolicyViolation},
		{"fragmented_text", []byte{0x01, 0x81, 1, 2, 3, 4}, websocket.ClosePolicyViolation},
		{"fragmented_binary", []byte{0x02, 0x81, 1, 2, 3, 4}, websocket.ClosePolicyViolation},
		{"oversized_header_only", []byte{0x82, 0xfe, 0x04, 0x01, 1, 2, 3, 4}, websocket.CloseMessageTooBig},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := dialBrowserEventTestServer(t, browserEventTestServer())
			if _, err := connection.UnderlyingConn().Write(test.header); err != nil {
				t.Fatal(err)
			}
			_, _, err := connection.ReadMessage()
			if !websocket.IsCloseError(err, test.closeCode) {
				t.Fatalf("header-only data returned %v, want close code %d before payload arrives", err, test.closeCode)
			}
		})
	}
}

func TestBrowserEventsControlFramesPreserveReplayAndLiveDelivery(t *testing.T) {
	server := browserEventTestServer()
	// The inbound limit must not truncate larger outbound event payloads.
	message := strings.Repeat("event", 1024)
	server.eventsAfter = func(context.Context, int64, int) ([]Event, error) {
		return []Event{{ID: 1, Type: "instance.log", ResourceType: "instance", ResourceID: "instance-a",
			Data: map[string]any{"node_id": "node-a", "line": message}}}, nil
	}
	connection := dialBrowserEventTestServer(t, server)
	var replay Event
	if err := connection.ReadJSON(&replay); err != nil || replay.ID != 1 || replay.Data["line"] != message {
		t.Fatalf("outbound replay was lost or truncated: id=%d err=%v", replay.ID, err)
	}
	pongReceived := make(chan string, 1)
	connection.SetPongHandler(func(payload string) error { pongReceived <- payload; return nil })
	if err := connection.WriteControl(websocket.PongMessage, []byte("heartbeat"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		event Event
		err   error
	}
	received := make(chan result, 1)
	go func() {
		var event Event
		err := connection.ReadJSON(&event)
		received <- result{event: event, err: err}
	}()
	select {
	case payload := <-pongReceived:
		if payload != "probe" {
			t.Fatalf("unexpected pong payload %q", payload)
		}
	case result := <-received:
		t.Fatalf("control frame interrupted event stream: %v", result.err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not respond to ping")
	}
	if _, err := server.events.Publish(context.Background(), Event{Type: "instance.log", ResourceType: "instance", ResourceID: "instance-a",
		Data: map[string]any{"node_id": "node-a", "line": message}}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-received:
		if result.err != nil || result.event.ID != 0 || result.event.Data["line"] != message {
			t.Fatalf("live event after control frames was lost or truncated: %v", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live event was not delivered")
	}
	if err := connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err := connection.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("normal client close returned %v", err)
	}
}
