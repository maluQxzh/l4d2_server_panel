package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

type agentConnection struct {
	conn         *websocket.Conn
	mu           sync.Mutex
	ownershipMu  sync.Mutex
	ownership    map[string]time.Time
	sent         map[string]struct{}
	canceled     map[string]struct{}
	capabilities map[string]struct{}
	jobUpdates   map[string]acceptedJobUpdate
}

var serverConfigNativeJobTypes = map[string]bool{
	"create": true, "import": true, "game.import": true, "install": true,
	"reinstall": true, "configure": true, "start": true, "restart": true,
	"restore": true, "update": true, "validate": true,
}

func (c *agentConnection) writeJSON(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeJSONLocked(value)
}

func (c *agentConnection) writeJSONLocked(value any) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(value)
}

type AgentHub struct {
	store             *Store
	events            *EventBroker
	log               *slog.Logger
	masterKey         []byte
	validateInstances func(context.Context, string, []string) error
	now               func() time.Time
	mu                sync.RWMutex
	conns             map[string]*agentConnection
	upgrader          websocket.Upgrader
}

func NewAgentHub(store *Store, events *EventBroker, logger *slog.Logger, masterKey []byte) *AgentHub {
	hub := &AgentHub{
		store: store, events: events, log: logger, masterKey: masterKey, conns: make(map[string]*agentConnection),
		now:      time.Now,
		upgrader: websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: func(*http.Request) bool { return true }},
	}
	if store != nil {
		hub.validateInstances = func(ctx context.Context, nodeID string, instanceIDs []string) error {
			owned, err := store.NodeOwnsInstances(ctx, nodeID, instanceIDs)
			if err != nil {
				return err
			}
			if !owned {
				return ErrUnauthorized
			}
			return nil
		}
	}
	return hub
}

func (h *AgentHub) Connected(nodeID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[nodeID]
	return ok
}

func (h *AgentHub) Supports(nodeID, capability string) bool {
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client == nil {
		return false
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	_, ok := client.capabilities[capability]
	return ok
}

func (h *AgentHub) Disconnect(nodeID string) {
	h.mu.Lock()
	client := h.conns[nodeID]
	delete(h.conns, nodeID)
	h.mu.Unlock()
	if client != nil {
		_ = client.conn.Close()
	}
}

// Cancel asks an online Agent to stop an already-dispatched job. The canceled
// set also closes the race where Dispatch has loaded a job while the API is
// canceling it: either the job is written first and then its cancellation is
// written, or Dispatch observes the marker and never writes the job.
func (h *AgentHub) Cancel(nodeID, jobID string) (bool, error) {
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client == nil {
		return false, nil
	}
	envelope, err := protocol.NewEnvelope(protocol.TypeJobCancel, nodeID, jobID, nil)
	if err != nil {
		return false, err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.canceled[jobID] = struct{}{}
	delete(client.sent, jobID)
	if err := client.writeJSONLocked(envelope); err != nil {
		return false, err
	}
	return true, nil
}

func (h *AgentHub) Close() {
	h.mu.Lock()
	connections := make([]*agentConnection, 0, len(h.conns))
	for id, client := range h.conns {
		connections = append(connections, client)
		delete(h.conns, id)
	}
	h.mu.Unlock()
	for _, client := range connections {
		_ = client.conn.Close()
	}
}

func (h *AgentHub) Serve(node domain.Node, remoteAddress string, w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &agentConnection{conn: conn, ownership: make(map[string]time.Time), sent: make(map[string]struct{}), canceled: make(map[string]struct{}), capabilities: make(map[string]struct{}), jobUpdates: make(map[string]acceptedJobUpdate)}
	h.mu.Lock()
	if old := h.conns[node.ID]; old != nil {
		_ = old.conn.Close()
	}
	h.conns[node.ID] = client
	h.mu.Unlock()
	defer func() {
		_ = conn.Close()
		h.mu.Lock()
		if h.conns[node.ID] == client {
			delete(h.conns, node.ID)
			_ = h.store.SetNodeOffline(context.Background(), node.ID)
			_ = h.store.CreateNotification(context.Background(), "critical", "node.offline", "Node offline",
				node.Name+" disconnected from the control plane", node.ID, nil)
			_, _ = h.events.Publish(context.Background(), Event{Type: "node.offline", ResourceType: "node", ResourceID: node.ID, Data: map[string]any{"node_id": node.ID}}, true)
		}
		h.mu.Unlock()
	}()
	_ = h.store.SetNodeConnected(r.Context(), node.ID, remoteAddress, node.AgentVersion)
	_, _ = h.events.Publish(r.Context(), Event{Type: "node.online", ResourceType: "node", ResourceID: node.ID, Data: map[string]any{"node_id": node.ID}}, true)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			h.Dispatch(ctx, node.ID)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	// Managed file transfers are capped at 8 MiB before Base64 encoding. Keep a
	// hard WebSocket ceiling above that encoded payload while still rejecting
	// unbounded messages from a compromised node.
	conn.SetReadLimit(16 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	})
	for {
		var envelope protocol.Envelope
		if err := conn.ReadJSON(&envelope); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
		if !protocol.CompatibleVersion(envelope.Version) || (envelope.NodeID != "" && envelope.NodeID != node.ID) {
			h.log.Warn("ignored invalid agent envelope", "node_id", node.ID, "version", envelope.Version)
			continue
		}
		if err := h.handleEnvelope(r.Context(), node, envelope); err != nil {
			h.log.Warn("agent message failed", "node_id", node.ID, "type", envelope.Type, "error", err)
			if envelope.Type == protocol.TypeJobUpdate {
				// A terminal result is replayed from the Agent journal after the
				// connection is re-established. Closing here turns projection or
				// event persistence failures into an automatic durable retry.
				return
			}
		}
	}
}

func (h *AgentHub) handleEnvelope(ctx context.Context, node domain.Node, envelope protocol.Envelope) error {
	switch envelope.Type {
	case protocol.TypeHello:
		var hello protocol.Hello
		if err := json.Unmarshal(envelope.Payload, &hello); err != nil {
			return err
		}
		h.mu.RLock()
		client := h.conns[node.ID]
		h.mu.RUnlock()
		if client != nil {
			client.mu.Lock()
			client.capabilities = make(map[string]struct{}, len(hello.Capabilities))
			for _, capability := range hello.Capabilities {
				capability = strings.TrimSpace(capability)
				if capability != "" {
					client.capabilities[capability] = struct{}{}
				}
			}
			client.mu.Unlock()
		}
		return h.store.SetNodeConnected(ctx, node.ID, "", hello.AgentVersion)
	case protocol.TypeHeartbeat:
		if len(envelope.Payload) > maxAgentHeartbeatPayload {
			return errors.New("heartbeat payload is too large")
		}
		var heartbeat protocol.Heartbeat
		if err := json.Unmarshal(envelope.Payload, &heartbeat); err != nil {
			return err
		}
		instanceIDs, err := validateHeartbeatPayload(&heartbeat, len(envelope.Payload))
		if err != nil {
			return err
		}
		if err := h.requireNodeInstances(ctx, node.ID, instanceIDs); err != nil {
			return err
		}
		newlyDegraded := make([]string, 0)
		crashLoops := make([]string, 0)
		for instanceID, status := range heartbeat.Instances {
			normalized := strings.ToLower(strings.TrimSpace(status))
			if normalized != "crash_loop" && normalized != "crash-loop" {
				continue
			}
			crashLoops = append(crashLoops, instanceID)
			if instance, lookupErr := h.store.GetInstance(ctx, instanceID); lookupErr == nil && instance.NodeID == node.ID && instance.Status != domain.InstanceDegraded {
				newlyDegraded = append(newlyDegraded, instanceID)
			}
			heartbeat.Instances[instanceID] = string(domain.InstanceDegraded)
		}
		if err := h.store.Heartbeat(ctx, node.ID, heartbeat.AgentVersion, heartbeat.Instances); err != nil {
			return err
		}
		for _, instanceID := range crashLoops {
			_ = h.store.CreateNotificationOnce(ctx, "critical", "instance.crash_loop", "Instance crash loop detected",
				"The Agent stopped automatic restarts after repeated crashes", instanceID,
				map[string]any{"instance_id": instanceID, "node_id": node.ID}, 6*time.Hour)
		}
		for _, instanceID := range newlyDegraded {
			_, _ = h.events.Publish(ctx, Event{Type: "instance.degraded", ResourceType: "instance", ResourceID: instanceID,
				Data: map[string]any{"instance_id": instanceID, "node_id": node.ID, "status": domain.InstanceDegraded, "reason": "crash_loop"}}, true)
		}
		return nil
	case protocol.TypeMetrics:
		if len(envelope.Payload) > maxAgentMetricsPayload {
			return errors.New("metrics payload is too large")
		}
		var samples []domain.MetricSample
		if err := json.Unmarshal(envelope.Payload, &samples); err != nil {
			var one domain.MetricSample
			if singleErr := json.Unmarshal(envelope.Payload, &one); singleErr != nil {
				return err
			}
			samples = []domain.MetricSample{one}
		}
		instanceIDs, err := validateMetricSamples(samples, len(envelope.Payload), time.Now().UTC())
		if err != nil {
			return err
		}
		if err := h.requireNodeInstances(ctx, node.ID, instanceIDs); err != nil {
			return err
		}
		for _, sample := range samples {
			sample.NodeID = node.ID
			if err := h.store.SaveMetric(ctx, sample); err != nil {
				return err
			}
			if sample.InstanceID == "" && sample.DiskTotal > 0 {
				usedPercent := float64(sample.DiskUsed) / float64(sample.DiskTotal) * 100
				if usedPercent >= 95 {
					_ = h.store.CreateNotificationOnce(ctx, "critical", "node.disk.critical", "Node disk critically full",
						"Disk use is above 95%", node.ID, map[string]any{"percent": usedPercent}, 6*time.Hour)
				} else if usedPercent >= 85 {
					_ = h.store.CreateNotificationOnce(ctx, "warning", "node.disk.warning", "Node disk nearly full",
						"Disk use is above 85%", node.ID, map[string]any{"percent": usedPercent}, 6*time.Hour)
				}
			}
			_, _ = h.events.Publish(ctx, Event{Type: "metric", ResourceType: "node", ResourceID: node.ID, Data: map[string]any{"sample": sample}}, false)
		}
		return nil
	case protocol.TypeJobUpdate:
		if len(envelope.Payload) > maxAgentTerminalJobPayload {
			return errors.New("job update payload is too large")
		}
		var update protocol.JobUpdate
		if err := json.Unmarshal(envelope.Payload, &update); err != nil {
			return err
		}
		job, err := h.store.GetJob(ctx, envelope.JobID)
		if err != nil || job.NodeID != node.ID {
			return ErrUnauthorized
		}
		if err := validateJobUpdate(job, node.ID, envelope, update); err != nil {
			return err
		}
		if job.InstanceID != "" {
			if err := h.requireNodeInstances(ctx, node.ID, []string{job.InstanceID}); err != nil {
				return err
			}
		}
		if latePendingCancellationUpdate(job, update.Status) {
			// A running/progress frame may already be in flight when the operator's
			// cancellation commits. It cannot revive the job and is not a protocol
			// fault; the durable cancellation outbox will keep requesting the real
			// terminal outcome.
			return nil
		}
		terminalFingerprint := ""
		terminalReplay := false
		if isTerminalJobStatus(update.Status) {
			for _, field := range []string{terminalOutcomeFingerprintField, jobProjectionPendingField} {
				if _, reserved := update.Result[field]; reserved {
					h.auditTerminalReplayRejection(ctx, node, job, update.Status, "reserved_result_field")
					return errReservedTerminalField
				}
			}
			terminalFingerprint, err = terminalJobUpdateFingerprint(h.masterKey, job.ID, update)
			if err != nil {
				return err
			}
			if settledTerminalJob(job) {
				equivalent, compareErr := terminalReplayEquivalent(h.masterKey, job, update, terminalFingerprint)
				if compareErr != nil || !equivalent {
					h.auditTerminalReplayRejection(ctx, node, job, update.Status, "outcome_mismatch")
					return errTerminalOutcomeConflict
				}
				terminalReplay = true
			}
		}
		if !h.acceptJobUpdate(node.ID, job.ID, update, time.Now().UTC()) {
			return nil
		}
		updated := job
		if !terminalReplay {
			if err := sealJobResultSecrets(h.masterKey, job.Type, update.Result); err != nil {
				return err
			}
			if terminalFingerprint != "" {
				if update.Result == nil {
					update.Result = make(map[string]any)
				}
				update.Result[terminalOutcomeFingerprintField] = terminalFingerprint
			}
			updated, err = h.store.UpdateJob(ctx, job.ID, update.Status, update.Progress, update.Message, update.Result)
			if errors.Is(err, errLateCanceledJobUpdate) {
				// CancelJob may win after the optimistic GetJob above. Treat that
				// interleaving exactly like the fast-path stale frame check.
				return nil
			}
			if errors.Is(err, errTerminalOutcomeConflict) {
				h.auditTerminalReplayRejection(ctx, node, job, update.Status, "concurrent_outcome_mismatch")
				return err
			}
			if err != nil {
				return err
			}
		}
		if jobProjectionPending(updated) {
			updated, _, err = h.store.ApplyPendingJobProjection(ctx, updated.ID)
			if err != nil {
				return fmt.Errorf("project terminal job: %w", err)
			}
		}
		if updated.Status == domain.JobSucceeded || updated.Status == domain.JobFailed || updated.Status == domain.JobCanceled {
			h.forgetCompleted(node.ID, updated.ID)
		}
		if updated.Status == domain.JobSucceeded && job.InstanceID != "" && (updated.Type == "delete" || updated.Type == "detach") {
			// The projection has removed the instance (and cascading instance role
			// bindings). Evict Agent ownership immediately and make browser streams
			// revalidate before their next frame.
			h.invalidateInstanceOwnership(job.InstanceID)
			h.events.InvalidateAuthorization()
		}
		if updated.Status == domain.JobFailed {
			metadata := map[string]any{"job_id": job.ID, "instance_id": job.InstanceID, "type": job.Type}
			if job.RequestedBy == "" && (job.Type == "a2s.info" || job.Type == "check_update") {
				_ = h.store.CreateNotificationOnce(ctx, "warning", "monitoring.probe_failed", "Monitoring probe failed",
					update.Message, job.InstanceID, metadata, time.Hour)
			} else {
				_ = h.store.CreateNotificationOnce(ctx, "warning", "job.failed", "Task failed", update.Message, job.ID, metadata, 24*time.Hour)
			}
		}
		if isTerminalJobStatus(updated.Status) && jobChangesInstance(updated.Type) && job.InstanceID != "" {
			instance, instanceErr := h.store.GetInstance(ctx, job.InstanceID)
			if instanceErr == nil {
				_, _ = h.events.Publish(ctx, Event{Type: "instance.updated", ResourceType: "instance", ResourceID: instance.ID,
					Data: map[string]any{"instance": instance, "job_id": updated.ID}}, true)
			} else if errors.Is(instanceErr, ErrNotFound) && updated.Status == domain.JobSucceeded && (updated.Type == "delete" || updated.Type == "detach") {
				_, _ = h.events.Publish(ctx, Event{Type: "instance.deleted", ResourceType: "instance", ResourceID: job.InstanceID,
					Data: map[string]any{"instance_id": job.InstanceID, "node_id": job.NodeID, "job_id": updated.ID}}, true)
			}
		}
		eventJob := updated
		eventJob.Payload, eventJob.Result = cloneMap(updated.Payload), cloneMap(updated.Result)
		stripJobFields(eventJob.Type, eventJob.Payload, eventJob.Result)
		if _, err = h.events.Publish(ctx, Event{Type: "job.updated", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": eventJob}}, true); err != nil {
			return err
		}
		if isTerminalJobStatus(updated.Status) {
			ack, err := protocol.NewEnvelope(protocol.TypeJobAck, node.ID, updated.ID, nil)
			if err != nil {
				return err
			}
			h.mu.RLock()
			client := h.conns[node.ID]
			h.mu.RUnlock()
			if client == nil {
				return errors.New("agent disconnected before final job acknowledgement")
			}
			if err := client.writeJSON(ack); err != nil {
				return fmt.Errorf("send final job acknowledgement: %w", err)
			}
			// A completed automatic maintenance update releases the node-level
			// dispatch gate. Re-scan after the ACK so the next durable intent does
			// not wait for a reconnect or another housekeeping tick.
			if isAutomaticMaintenanceUpdate(updated) {
				h.dispatchSoon(node.ID)
			}
		}
		return nil
	case protocol.TypeLog:
		if len(envelope.Payload) > maxAgentLogPayload {
			return errors.New("log payload is too large")
		}
		var event protocol.LogEvent
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return err
		}
		instanceIDs, err := validateLogEvent(&event, len(envelope.Payload), time.Now().UTC())
		if err != nil {
			return err
		}
		if err := h.requireNodeInstances(ctx, node.ID, instanceIDs); err != nil {
			return err
		}
		_, err = h.events.Publish(ctx, Event{Type: "instance.log", ResourceType: "instance", ResourceID: event.InstanceID,
			Data: map[string]any{"node_id": node.ID, "instance_id": event.InstanceID, "stream": event.Stream, "line": event.Line, "at": event.At}}, false)
		return err
	default:
		return errors.New("unsupported agent message type")
	}
}

func (h *AgentHub) auditTerminalReplayRejection(ctx context.Context, node domain.Node, job domain.Job, reported domain.JobStatus, reason string) {
	_ = h.store.WriteAudit(ctx, domain.AuditEvent{
		ActorName: "agent:" + node.ID, Action: "job.terminal_replay_rejected", ResourceType: "job", ResourceID: job.ID,
		Metadata: map[string]any{"node_id": node.ID, "instance_id": job.InstanceID, "stored_status": job.Status,
			"reported_status": reported, "reason": reason},
	})
}

func (h *AgentHub) forgetSent(nodeID, jobID string) {
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client != nil {
		client.mu.Lock()
		delete(client.sent, jobID)
		client.mu.Unlock()
	}
}

func (h *AgentHub) forgetCompleted(nodeID, jobID string) {
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client != nil {
		client.mu.Lock()
		delete(client.sent, jobID)
		delete(client.canceled, jobID)
		delete(client.jobUpdates, jobID)
		client.mu.Unlock()
	}
}

func (h *AgentHub) Dispatch(ctx context.Context, nodeID string) {
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client == nil {
		return
	}
	jobs, err := h.store.JobsForDispatch(ctx, nodeID, 20)
	if err != nil {
		h.log.Error("load dispatch jobs", "node_id", nodeID, "error", err)
		return
	}
	for _, job := range jobs {
		client.mu.Lock()
		_, sent := client.sent[job.ID]
		_, canceled := client.canceled[job.ID]
		_, runtimeEnvironmentSupported := client.capabilities["runtime_environment_v1"]
		_, absoluteGamePathsSupported := client.capabilities["absolute_game_paths"]
		_, serverConfigNativeSupported := client.capabilities["server_cfg_native"]
		_, qqFlashFileSupported := client.capabilities["qq_flash_file_v1"]
		client.mu.Unlock()
		if job.Status != domain.JobCanceled && (sent || canceled) {
			continue
		}
		if environmentJobTypes[job.Type] && !runtimeEnvironmentSupported {
			// A node may reconnect with an older Agent after a job was queued.
			// Leave the durable intent queued until a compatible Agent returns.
			continue
		}
		if job.Type == "file" && strings.EqualFold(stringFromAny(job.Payload["operation"]), "qq_flash") && !qqFlashFileSupported {
			// A queued import may outlive a temporary Agent rollback. Keep the
			// expiring share intent durable until a compatible Agent reconnects.
			continue
		}
		if job.InstanceID != "" && serverConfigNativeJobTypes[job.Type] && !serverConfigNativeSupported {
			// Older Agents still generate and inject l4d2panel.cfg. Keep these
			// jobs queued until an Agent advertising the administrator-owned
			// server.cfg launch contract connects.
			continue
		}
		if !absoluteGamePathsSupported {
			usesAbsolutePath := jobPayloadContainsAbsoluteGamePath(job.Payload)
			if job.InstanceID != "" {
				if instance, instanceErr := h.store.GetInstance(ctx, job.InstanceID); instanceErr == nil {
					usesAbsolutePath = usesAbsolutePath || absoluteGamePathSyntax(instance.Path)
				}
			}
			if usesAbsolutePath {
				// Keep the durable intent queued if a node rolls back to an Agent
				// that predates absolute-path installation support.
				continue
			}
		}
		if job.Status == domain.JobCanceled {
			// A cancel_pending row is the durable cancellation outbox. Re-send it
			// on every dispatch pass (and after reconnect) until the Agent reports
			// its real terminal outcome and the marker is cleared. This closes the
			// MarkDispatched-before-WebSocket-write gap without guessing whether
			// the Agent ever claimed the operation.
			envelope, envelopeErr := protocol.NewEnvelope(protocol.TypeJobCancel, nodeID, job.ID, nil)
			if envelopeErr != nil {
				return
			}
			client.mu.Lock()
			// Re-read while holding the same connection write lock used by the
			// terminal acknowledgement. If a stale dispatch snapshot races a
			// late success, either this check observes the terminal DB state and
			// skips, or the cancel frame is ordered on the wire before its ACK.
			current, currentErr := h.store.GetJob(ctx, job.ID)
			cancelStillPending, _ := current.Result["cancel_pending"].(bool)
			if currentErr != nil || current.Status != domain.JobCanceled || !cancelStillPending {
				client.mu.Unlock()
				continue
			}
			writeErr := client.writeJSONLocked(envelope)
			client.mu.Unlock()
			if writeErr != nil {
				return
			}
			continue
		}
		outbound := job
		outbound.Payload = cloneMap(job.Payload)
		if err := unsealJobPayloadSecrets(h.masterKey, job.Type, outbound.Payload); err != nil {
			h.log.Error("decrypt queued job payload", "job_id", job.ID, "error", err)
			continue
		}
		secretActions := map[string]bool{"create": true, "import": true, "game.import": true, "install": true, "reinstall": true, "configure": true, "start": true, "stop": true, "restart": true, "restore": true,
			"update": true, "validate": true, "delete": true, "rcon": true, "kick_player": true, "ban_player": true,
			"unban_player": true, "change_map": true, "rcon.players": true, "install_mod": true, "install_package": true, "rollback_package": true}
		if job.InstanceID != "" && secretActions[job.Type] {
			if rconCiphertext, _, secretErr := h.store.InstanceSecrets(ctx, job.InstanceID); secretErr == nil {
				if secret, decryptErr := decryptSecret(h.masterKey, rconCiphertext); decryptErr == nil {
					outbound.Payload["rcon_password"] = secret
				}
			}
		}
		envelope, err := protocol.NewEnvelope(protocol.TypeJob, nodeID, job.ID, protocol.JobCommand{Job: outbound})
		if err != nil {
			return
		}
		if job.Status == domain.JobQueued {
			claimed, err := h.store.MarkDispatched(ctx, job.ID)
			if err != nil {
				return
			}
			if !claimed {
				continue
			}
		}
		client.mu.Lock()
		if _, canceled := client.canceled[job.ID]; canceled {
			client.mu.Unlock()
			continue
		}
		if client.writeJSONLocked(envelope) != nil {
			client.mu.Unlock()
			return
		}
		client.sent[job.ID] = struct{}{}
		client.mu.Unlock()
		if job.Status == domain.JobQueued {
			_, _ = h.events.Publish(ctx, Event{Type: "job.dispatched", ResourceType: "job", ResourceID: job.ID,
				Data: map[string]any{"job_id": job.ID, "node_id": job.NodeID, "instance_id": job.InstanceID}}, true)
		}
	}
}

func jobPayloadContainsAbsoluteGamePath(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "path" {
				if path, ok := child.(string); ok && absoluteGamePathSyntax(path) {
					return true
				}
			}
			if jobPayloadContainsAbsoluteGamePath(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if jobPayloadContainsAbsoluteGamePath(child) {
				return true
			}
		}
	}
	return false
}

func (h *AgentHub) dispatchSoon(nodeID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.Dispatch(ctx, nodeID)
	}()
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func isTerminalJobStatus(status domain.JobStatus) bool {
	return status == domain.JobSucceeded || status == domain.JobFailed || status == domain.JobCanceled
}

func jobChangesInstance(jobType string) bool {
	switch jobType {
	case "start", "stop", "restart", "install", "reinstall", "import", "restore", "configure", "update", "validate", "a2s.info", "delete", "detach", "install_mod", "toggle_package", "rollback_package":
		return true
	default:
		return false
	}
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}
