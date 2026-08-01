package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestLiveEventAuthorizationQueryCountIsIndependentOfEventRate(t *testing.T) {
	broker := NewEventBroker(nil)
	var revalidations atomic.Int32
	var revoked atomic.Bool
	server := &Server{
		events:                   broker,
		eventAuthRefreshInterval: time.Hour,
		eventCatchupInterval:     time.Hour,
		revalidateEventPrincipal: func(_ context.Context, principal Principal) (Principal, error) {
			revalidations.Add(1)
			if revoked.Load() {
				return Principal{}, ErrUnauthorized
			}
			return principal, nil
		},
	}
	state := eventAuthorizationState{
		principal:  Principal{User: domain.User{Role: domain.RoleOwner}, ExpiresAt: time.Now().Add(time.Hour)},
		generation: broker.AuthorizationGeneration(),
	}
	event := Event{Type: "instance.log", ResourceType: "instance", ResourceID: "instance-a",
		Data: map[string]any{"node_id": "node-a", "instance_id": "instance-a", "line": "ready"}}
	for range 10_000 {
		allowed, err := server.authorizeStreamEvent(context.Background(), &state, event)
		if err != nil || !allowed {
			t.Fatalf("live event was rejected: allowed=%v err=%v", allowed, err)
		}
	}
	if got := revalidations.Load(); got != 0 {
		t.Fatalf("10,000 live events caused %d principal queries, want 0", got)
	}
	broker.InvalidateAuthorization()
	if allowed, err := server.authorizeStreamEvent(context.Background(), &state, event); err != nil || !allowed {
		t.Fatalf("event after authorization invalidation was rejected: allowed=%v err=%v", allowed, err)
	}
	if got := revalidations.Load(); got != 1 {
		t.Fatalf("authorization invalidation caused %d principal queries, want 1", got)
	}
	revoked.Store(true)
	broker.InvalidateAuthorization()
	if allowed, err := server.authorizeStreamEvent(context.Background(), &state, event); err == nil || allowed {
		t.Fatalf("revoked stream remained authorized: allowed=%v err=%v", allowed, err)
	}
	if got := revalidations.Load(); got != 2 {
		t.Fatalf("revocation caused %d total principal queries, want 2", got)
	}
}

func TestWebSocketRefreshesMiddlewarePrincipalBeforeInitialReplay(t *testing.T) {
	broker := NewEventBroker(nil)
	// Model a credential revoked after authenticate middleware produced the
	// request principal but before handleEvents sampled the current generation.
	broker.InvalidateAuthorization()
	var revalidations atomic.Int32
	var replayQueries atomic.Int32
	server := &Server{
		events:                   broker,
		eventAuthRefreshInterval: time.Hour,
		eventCatchupInterval:     time.Hour,
		revalidateEventPrincipal: func(_ context.Context, _ Principal) (Principal, error) {
			revalidations.Add(1)
			return Principal{}, ErrUnauthorized
		},
		eventsAfter: func(_ context.Context, _ int64, _ int) ([]Event, error) {
			replayQueries.Add(1)
			return []Event{{ID: 1, Type: "node.updated", ResourceType: "node", ResourceID: "node-a"}}, nil
		},
	}
	connection, cleanup := dialEventHandler(t, server, Principal{
		User: domain.User{Role: domain.RoleOwner}, ExpiresAt: time.Now().Add(time.Hour),
	})
	defer cleanup()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var event Event
	err := connection.ReadJSON(&event)
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("revoked middleware snapshot received replay or wrong close: event=%#v err=%v", event, err)
	}
	if got := revalidations.Load(); got != 1 {
		t.Fatalf("initial connection performed %d principal refreshes, want 1", got)
	}
	if got := replayQueries.Load(); got != 0 {
		t.Fatalf("revoked connection queried/delivered replay %d times", got)
	}
}

func TestWebSocketRechecksGenerationChangedDuringInitialRefresh(t *testing.T) {
	broker := NewEventBroker(nil)
	firstRefreshEntered := make(chan struct{})
	releaseFirstRefresh := make(chan struct{})
	var revalidations atomic.Int32
	server := &Server{
		events:                   broker,
		eventAuthRefreshInterval: time.Hour,
		eventCatchupInterval:     time.Hour,
		revalidateEventPrincipal: func(_ context.Context, principal Principal) (Principal, error) {
			if revalidations.Add(1) == 1 {
				close(firstRefreshEntered)
				<-releaseFirstRefresh
				return principal, nil
			}
			return Principal{}, ErrUnauthorized
		},
		eventsAfter: func(_ context.Context, _ int64, _ int) ([]Event, error) {
			return []Event{{ID: 1, Type: "node.updated", ResourceType: "node", ResourceID: "node-a"}}, nil
		},
	}
	connection, cleanup := dialEventHandler(t, server, Principal{
		User: domain.User{Role: domain.RoleOwner}, ExpiresAt: time.Now().Add(time.Hour),
	})
	defer cleanup()
	<-firstRefreshEntered
	broker.InvalidateAuthorization()
	close(releaseFirstRefresh)
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var event Event
	err := connection.ReadJSON(&event)
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("generation changed during refresh leaked replay: event=%#v err=%v", event, err)
	}
	if got := revalidations.Load(); got != 2 {
		t.Fatalf("generation race caused %d refreshes, want 2", got)
	}
}

func TestWebSocketPersistentCursorGapIsCaughtUpWithoutPerLiveEventQueries(t *testing.T) {
	broker := NewEventBroker(nil)
	var replayQueries atomic.Int32
	initialReplayDone := make(chan struct{})
	server := &Server{
		events:                   broker,
		eventAuthRefreshInterval: time.Hour,
		eventCatchupInterval:     time.Hour,
		revalidateEventPrincipal: func(_ context.Context, principal Principal) (Principal, error) { return principal, nil },
	}
	server.eventsAfter = func(_ context.Context, cursor int64, _ int) ([]Event, error) {
		query := replayQueries.Add(1)
		if query == 1 {
			close(initialReplayDone)
			return nil, nil
		}
		if cursor == 0 {
			return []Event{
				{ID: 1, Type: "node.updated", ResourceType: "node", ResourceID: "node-a", CreatedAt: time.Now().UTC()},
				{ID: 2, Type: "node.updated", ResourceType: "node", ResourceID: "node-a", CreatedAt: time.Now().UTC()},
			}, nil
		}
		return nil, nil
	}

	connection, cleanup := dialEventHandler(t, server, Principal{
		User: domain.User{Role: domain.RoleOwner}, ExpiresAt: time.Now().Add(time.Hour),
	})
	defer cleanup()
	<-initialReplayDone
	// ID 1 models a persistent broker notification dropped by a full channel.
	// Seeing ID 2 must query the durable cursor and deliver both exactly once.
	broker.broadcast(Event{ID: 2, Type: "node.updated", ResourceType: "node", ResourceID: "node-a", CreatedAt: time.Now().UTC()})
	for want := int64(1); want <= 2; want++ {
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		var event Event
		if err := connection.ReadJSON(&event); err != nil {
			t.Fatalf("read caught-up event %d: %v", want, err)
		}
		if event.ID != want {
			t.Fatalf("caught-up event ID=%d, want %d", event.ID, want)
		}
	}
	if got := replayQueries.Load(); got != 2 {
		t.Fatalf("one cursor gap caused %d replay queries including initial replay, want 2", got)
	}
}

func TestWebSocketPeriodicCatchupRecoversDroppedTailEvent(t *testing.T) {
	broker := NewEventBroker(nil)
	var replayQueries atomic.Int32
	var tailAvailable atomic.Bool
	initialReplayDone := make(chan struct{})
	server := &Server{
		events:                   broker,
		eventAuthRefreshInterval: time.Hour,
		eventCatchupInterval:     20 * time.Millisecond,
		revalidateEventPrincipal: func(_ context.Context, principal Principal) (Principal, error) { return principal, nil },
	}
	server.eventsAfter = func(_ context.Context, cursor int64, _ int) ([]Event, error) {
		query := replayQueries.Add(1)
		if query == 1 {
			close(initialReplayDone)
			return nil, nil
		}
		if tailAvailable.Load() && cursor == 0 {
			return []Event{{ID: 1, Type: "node.updated", ResourceType: "node", ResourceID: "node-a", CreatedAt: time.Now().UTC()}}, nil
		}
		return nil, nil
	}
	connection, cleanup := dialEventHandler(t, server, Principal{
		User: domain.User{Role: domain.RoleOwner}, ExpiresAt: time.Now().Add(time.Hour),
	})
	defer cleanup()
	<-initialReplayDone
	tailAvailable.Store(true)
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var event Event
	if err := connection.ReadJSON(&event); err != nil {
		t.Fatalf("periodic catch-up did not recover the dropped tail: %v", err)
	}
	if event.ID != 1 || replayQueries.Load() < 2 {
		t.Fatalf("unexpected periodic catch-up: event=%#v queries=%d", event, replayQueries.Load())
	}
}

func TestEventBrokerOverflowLeavesDetectablePersistentGap(t *testing.T) {
	broker := NewEventBroker(nil)
	live, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	for id := int64(1); id <= 129; id++ {
		broker.broadcast(Event{ID: id})
	}
	lastID := int64(0)
	for range 128 {
		lastID = (<-live).ID
	}
	broker.broadcast(Event{ID: 130})
	next := <-live
	if lastID != 128 || next.ID != 130 || next.ID <= lastID+1 {
		t.Fatalf("overflow did not leave a detectable cursor gap: last=%d next=%d", lastID, next.ID)
	}
}

func TestWriteEventJSONRefreshesStaleDeadline(t *testing.T) {
	writeResult := make(chan error, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, request, nil)
		if err != nil {
			writeResult <- err
			return
		}
		defer connection.Close()
		_ = connection.SetWriteDeadline(time.Now().Add(-time.Second))
		writeResult <- writeEventJSON(connection, map[string]any{"ok": true})
	}))
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var response map[string]any
	if err := connection.ReadJSON(&response); err != nil {
		t.Fatalf("read after stale server write deadline: %v", err)
	}
	if err := <-writeResult; err != nil {
		t.Fatalf("writeEventJSON did not refresh the stale deadline: %v", err)
	}
}

func dialEventHandler(t *testing.T, server *Server, principal Principal) (*websocket.Conn, func()) {
	t.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), principalContextKey, principal)
		server.handleEvents(w, request.WithContext(ctx))
	}))
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		httpServer.Close()
		t.Fatalf("dial event WebSocket: %v", err)
	}
	return connection, func() {
		_ = connection.Close()
		httpServer.Close()
	}
}
