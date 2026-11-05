package control

import (
	"context"
	"sync"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type nodeLifecycleLock struct {
	mu   sync.Mutex
	refs int
}

// Serializes registration/offline projection for one node while keeping the
// fleet registry lock free of database and socket I/O. Entries are refcounted.
func (h *AgentHub) lockNodeLifecycle(id string) func() {
	h.mu.Lock()
	if h.lifecycle == nil {
		h.lifecycle = make(map[string]*nodeLifecycleLock)
	}
	lock := h.lifecycle[id]
	if lock == nil {
		lock = &nodeLifecycleLock{}
		h.lifecycle[id] = lock
	}
	lock.refs++
	h.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		h.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(h.lifecycle, id)
		}
		h.mu.Unlock()
	}
}

func (h *AgentHub) disconnectSession(node domain.Node, client *agentConnection) {
	_ = client.conn.Close()
	unlock := h.lockNodeLifecycle(node.ID)
	defer unlock()
	h.mu.Lock()
	current := h.conns[node.ID] == client
	if current {
		delete(h.conns, node.ID)
		delete(h.preparedQueueCursors, node.ID)
	}
	h.mu.Unlock()
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.store.SetNodeOffline(ctx, node.ID); err != nil {
		h.log.Warn("persist disconnected node", "node_id", node.ID, "error", err)
		return
	}
	_ = h.store.CreateNotification(ctx, "critical", "node.offline", "Node offline", node.Name+" disconnected from the control plane", node.ID, nil)
	_, _ = h.events.Publish(ctx, Event{Type: "node.offline", ResourceType: "node", ResourceID: node.ID, Data: map[string]any{"node_id": node.ID}}, true)
}
