package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 4096, CheckOrigin: sameOrigin}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	live, unsubscribe := s.events.Subscribe()
	defer unsubscribe()
	lastID := cursor
	authorization := eventAuthorizationState{
		principal:  principalFromContext(r.Context()),
		generation: s.events.AuthorizationGeneration(),
	}
	// Authentication middleware necessarily runs before the WebSocket upgrade.
	// Re-read the credential after upgrading so a revoke+bump in that window
	// cannot pair the stale principal with the newer generation and authorize the
	// initial replay. The generation protocol below closes a concurrent revoke
	// during this database read as well.
	initialAuthCtx, cancelInitialAuth := context.WithTimeout(r.Context(), 2*time.Second)
	initialAuthErr := s.refreshStreamAuthorization(initialAuthCtx, &authorization)
	cancelInitialAuth()
	if initialAuthErr != nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"), time.Now().Add(5*time.Second))
		return
	}
	lastDeliveredID := cursor
	const replayPageSize, replayConnectionLimit = 500, 5000
	replayEvents := s.eventsAfter
	if replayEvents == nil && s.store != nil {
		replayEvents = s.store.EventsAfter
	}
	catchUp := func() (bool, error) {
		if replayEvents == nil {
			return false, errors.New("event replay store is unavailable")
		}
		for replayed := 0; replayed < replayConnectionLimit; {
			replay, replayErr := replayEvents(r.Context(), lastID, replayPageSize)
			if replayErr != nil {
				return false, replayErr
			}
			for _, event := range replay {
				if event.ID <= lastID && lastID != 0 {
					continue
				}
				lastID = event.ID
				allowed, authErr := s.authorizeStreamEvent(r.Context(), &authorization, event)
				if authErr != nil {
					return false, authErr
				}
				if !allowed {
					continue
				}
				event = redactEventForPrincipal(event, authorization.principal)
				if writeErr := writeEventJSON(conn, event); writeErr != nil {
					return false, writeErr
				}
				lastDeliveredID = event.ID
			}
			// Advance scoped clients over filtered events as well. Otherwise a
			// reconnect could replay the same invisible window forever.
			if lastID > lastDeliveredID {
				marker := Event{ID: lastID, Type: "stream.cursor", CreatedAt: time.Now().UTC()}
				if writeErr := writeEventJSON(conn, marker); writeErr != nil {
					return false, writeErr
				}
				lastDeliveredID = lastID
			}
			replayed += len(replay)
			if len(replay) < replayPageSize {
				return false, nil
			}
		}
		return true, nil
	}
	if exhausted, replayErr := catchUp(); replayErr != nil {
		if errors.Is(replayErr, ErrUnauthorized) {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"), time.Now().Add(5*time.Second))
			return
		}
		_ = writeEventJSON(conn, map[string]any{"error": map[string]string{"code": "replay_failed", "message": "event replay failed"}})
		return
	} else if exhausted {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "continuing event replay"), time.Now().Add(5*time.Second))
		return
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()
	authRefreshInterval := s.eventAuthRefreshInterval
	if authRefreshInterval <= 0 {
		authRefreshInterval = time.Second
	}
	authTicker := time.NewTicker(authRefreshInterval)
	defer authTicker.Stop()
	catchupInterval := s.eventCatchupInterval
	if catchupInterval <= 0 {
		catchupInterval = 2 * time.Second
	}
	catchupTicker := time.NewTicker(catchupInterval)
	defer catchupTicker.Stop()
	expiresIn := time.Until(authorization.principal.ExpiresAt)
	if expiresIn < 0 {
		expiresIn = 0
	}
	expiryTimer := time.NewTimer(expiresIn)
	defer expiryTimer.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closed:
			return
		case <-expiryTimer.C:
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session expired"), time.Now().Add(5*time.Second))
			return
		case event, ok := <-live:
			if !ok {
				return
			}
			if event.ID > 0 && event.ID <= lastID {
				continue
			}
			// A full broker channel may discard notifications, including durable
			// ones. Persistent IDs expose that gap; replay it before the newer live
			// event. Ephemeral console/metric frames intentionally remain lossy.
			if event.ID > lastID+1 {
				exhausted, replayErr := catchUp()
				if replayErr != nil || exhausted {
					if errors.Is(replayErr, ErrUnauthorized) {
						_ = conn.WriteControl(websocket.CloseMessage,
							websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"), time.Now().Add(5*time.Second))
						return
					}
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "event catch-up failed"), time.Now().Add(5*time.Second))
					return
				}
				if event.ID <= lastID {
					continue
				}
			}
			allowed, authErr := s.authorizeStreamEvent(r.Context(), &authorization, event)
			if authErr != nil {
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"), time.Now().Add(5*time.Second))
				return
			}
			if !allowed {
				if event.ID > lastID {
					lastID = event.ID
				}
				continue
			}
			event = redactEventForPrincipal(event, authorization.principal)
			if err := writeEventJSON(conn, event); err != nil {
				return
			}
			if event.ID > lastID {
				lastID = event.ID
				lastDeliveredID = event.ID
			}
		case <-authTicker.C:
			checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			sessionErr := s.refreshStreamAuthorization(checkCtx, &authorization)
			cancel()
			if sessionErr != nil {
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"), time.Now().Add(5*time.Second))
				return
			}
		case <-catchupTicker.C:
			exhausted, replayErr := catchUp()
			if replayErr != nil || exhausted {
				if errors.Is(replayErr, ErrUnauthorized) {
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"), time.Now().Add(5*time.Second))
					return
				}
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "event catch-up failed"), time.Now().Add(5*time.Second))
				return
			}
		case <-pingTicker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func writeEventJSON(conn *websocket.Conn, value any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return conn.WriteJSON(value)
}

type eventAuthorizationState struct {
	principal  Principal
	generation uint64
}

func (s *Server) refreshStreamAuthorization(ctx context.Context, state *eventAuthorizationState) error {
	generation := s.events.AuthorizationGeneration()
	revalidate := s.revalidateEventPrincipal
	if revalidate == nil && s.store != nil {
		revalidate = s.store.RevalidatePrincipal
	}
	if revalidate == nil {
		return ErrUnauthorized
	}
	active, err := revalidate(ctx, state.principal)
	if err != nil {
		return err
	}
	state.principal = active
	// Record the generation observed before the database read. If a revocation
	// commits concurrently, the next event observes the newer generation and
	// revalidates again instead of accepting a stale snapshot.
	state.generation = generation
	return nil
}

func (s *Server) authorizeStreamEvent(ctx context.Context, state *eventAuthorizationState, event Event) (bool, error) {
	if state.generation != s.events.AuthorizationGeneration() {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := s.refreshStreamAuthorization(checkCtx, state)
		cancel()
		if err != nil {
			return false, err
		}
	}
	return s.eventAllowed(ctx, state.principal, event), nil
}

func redactEvent(event Event, role domain.Role) Event {
	return redactEventForPrincipal(event, Principal{User: domain.User{Role: role}})
}

func redactEventForPrincipal(event Event, principal Principal) Event {
	if event.Data == nil {
		return event
	}
	data := make(map[string]any, len(event.Data))
	for key, value := range event.Data {
		data[key] = value
	}
	if raw, ok := data["job"]; ok {
		switch job := raw.(type) {
		case domain.Job:
			data["job"] = redactJobForPrincipal(job, principal)
		case map[string]any:
			jobScope := domain.Job{NodeID: eventString(job, "node_id"), InstanceID: eventString(job, "instance_id")}
			canOperate := (jobScope.NodeID != "" || jobScope.InstanceID != "") && principal.AllowsJobPermission(jobScope, PermissionOperate)
			copyJob := make(map[string]any, len(job))
			for key, value := range job {
				if canOperate || (key != "payload" && key != "result") {
					copyJob[key] = value
				}
			}
			if canOperate {
				payload, _ := copyJob["payload"].(map[string]any)
				result, _ := copyJob["result"].(map[string]any)
				payload, result = cloneMap(payload), cloneMap(result)
				stripJobFields(eventString(copyJob, "type"), payload, result)
				copyJob["payload"], copyJob["result"] = payload, result
			}
			data["job"] = copyJob
		}
	}
	event.Data = data
	return event
}

func sameOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		return true
	}
	origin, err := url.Parse(raw)
	return err == nil && origin.Host == r.Host
}
