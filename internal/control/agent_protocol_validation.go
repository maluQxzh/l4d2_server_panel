package control

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

const (
	maxAgentHeartbeatPayload   = 256 << 10
	maxAgentHeartbeatInstances = 512
	maxAgentMetricsPayload     = 512 << 10
	maxAgentMetricSamples      = 512
	// JSON escaping can expand a valid 16 KiB line by up to six times.
	maxAgentLogPayload           = 128 << 10
	maxAgentLogLineBytes         = 16 << 10
	maxAgentProgressPayload      = 32 << 10
	maxAgentTerminalJobPayload   = 12 << 20
	maxAgentJobMessageBytes      = 2 << 10
	maxAgentMetricCPUPercent     = 10_000
	maxAgentMetricPlayers        = 256
	agentObservationPastWindow   = 15 * time.Minute
	agentObservationFutureWindow = 2 * time.Minute
	minAgentJobUpdateInterval    = 250 * time.Millisecond
	maxTrackedAgentJobUpdates    = 2_048
	agentJobUpdateTrackingWindow = time.Hour
	agentOwnershipCacheTTL       = 2 * time.Second
	maxAgentOwnershipCacheItems  = 1_024
)

var heartbeatInstanceStatuses = map[string]struct{}{
	"installing": {},
	"stopped":    {},
	"starting":   {},
	"running":    {},
	"stopping":   {},
	"updating":   {},
	"degraded":   {},
	"external":   {},
	"crash_loop": {},
	"crash-loop": {},
}

type acceptedJobUpdate struct {
	status   domain.JobStatus
	progress int
	message  string
	at       time.Time
}

func validateHeartbeatPayload(heartbeat *protocol.Heartbeat, payloadBytes int) ([]string, error) {
	if payloadBytes > maxAgentHeartbeatPayload {
		return nil, errors.New("heartbeat payload is too large")
	}
	if len(heartbeat.AgentVersion) > 128 {
		return nil, errors.New("agent version is too long")
	}
	if len(heartbeat.Instances) > maxAgentHeartbeatInstances {
		return nil, errors.New("heartbeat contains too many instances")
	}
	instanceIDs := make([]string, 0, len(heartbeat.Instances))
	for instanceID, status := range heartbeat.Instances {
		if err := validateAgentInstanceID(instanceID); err != nil {
			return nil, err
		}
		normalized := strings.ToLower(strings.TrimSpace(status))
		if _, ok := heartbeatInstanceStatuses[normalized]; !ok {
			return nil, fmt.Errorf("unsupported heartbeat status for instance %s", instanceID)
		}
		heartbeat.Instances[instanceID] = normalized
		instanceIDs = append(instanceIDs, instanceID)
	}
	return instanceIDs, nil
}

func validateMetricSamples(samples []domain.MetricSample, payloadBytes int, now time.Time) ([]string, error) {
	if payloadBytes > maxAgentMetricsPayload {
		return nil, errors.New("metrics payload is too large")
	}
	if len(samples) == 0 {
		return nil, errors.New("metrics payload is empty")
	}
	if len(samples) > maxAgentMetricSamples {
		return nil, errors.New("metrics payload contains too many samples")
	}
	seen := make(map[string]struct{}, len(samples))
	instanceIDs := make([]string, 0, len(samples))
	for index := range samples {
		sample := &samples[index]
		if _, duplicate := seen[sample.InstanceID]; duplicate {
			return nil, errors.New("metrics payload contains duplicate instance samples")
		}
		seen[sample.InstanceID] = struct{}{}
		if sample.InstanceID != "" {
			if err := validateAgentInstanceID(sample.InstanceID); err != nil {
				return nil, err
			}
			instanceIDs = append(instanceIDs, sample.InstanceID)
		}
		if math.IsNaN(sample.CPUPercent) || math.IsInf(sample.CPUPercent, 0) || sample.CPUPercent < 0 || sample.CPUPercent > maxAgentMetricCPUPercent {
			return nil, fmt.Errorf("metric sample %d has an invalid CPU percentage", index)
		}
		if sample.MemoryUsed > math.MaxInt64 || sample.MemoryTotal > math.MaxInt64 || sample.DiskUsed > math.MaxInt64 ||
			sample.DiskTotal > math.MaxInt64 || sample.NetworkRx > math.MaxInt64 || sample.NetworkTx > math.MaxInt64 {
			return nil, fmt.Errorf("metric sample %d exceeds the supported counter range", index)
		}
		if sample.MemoryTotal > 0 && sample.MemoryUsed > sample.MemoryTotal {
			return nil, fmt.Errorf("metric sample %d reports memory use above its total", index)
		}
		if sample.DiskTotal > 0 && sample.DiskUsed > sample.DiskTotal {
			return nil, fmt.Errorf("metric sample %d reports disk use above its total", index)
		}
		if sample.Players < 0 || sample.Players > maxAgentMetricPlayers {
			return nil, fmt.Errorf("metric sample %d has an invalid player count", index)
		}
		if err := normalizeAgentObservationTime(&sample.CollectedAt, now); err != nil {
			return nil, fmt.Errorf("metric sample %d: %w", index, err)
		}
	}
	return instanceIDs, nil
}

func validateLogEvent(event *protocol.LogEvent, payloadBytes int, now time.Time) ([]string, error) {
	if payloadBytes > maxAgentLogPayload {
		return nil, errors.New("log payload is too large")
	}
	if err := validateAgentInstanceID(event.InstanceID); err != nil {
		return nil, err
	}
	event.Stream = strings.ToLower(strings.TrimSpace(event.Stream))
	if event.Stream != "stdout" && event.Stream != "stderr" {
		return nil, errors.New("log stream must be stdout or stderr")
	}
	if len(event.Line) == 0 {
		return nil, errors.New("log line is empty")
	}
	if len(event.Line) > maxAgentLogLineBytes {
		return nil, errors.New("log line is too long")
	}
	if err := normalizeAgentObservationTime(&event.At, now); err != nil {
		return nil, err
	}
	return []string{event.InstanceID}, nil
}

func validateJobUpdate(job domain.Job, nodeID string, envelope protocol.Envelope, update protocol.JobUpdate) error {
	if len(envelope.Payload) > maxAgentTerminalJobPayload {
		return errors.New("job update payload is too large")
	}
	if envelope.JobID == "" || uuid.Validate(envelope.JobID) != nil {
		return errors.New("job update has an invalid job ID")
	}
	if job.ID != envelope.JobID || job.NodeID != nodeID {
		return ErrUnauthorized
	}
	if len(update.Message) > maxAgentJobMessageBytes {
		return errors.New("job update message is too long")
	}
	switch update.Status {
	case domain.JobRunning:
		if len(envelope.Payload) > maxAgentProgressPayload {
			return errors.New("running job update payload is too large")
		}
		if update.Result != nil {
			return errors.New("non-terminal job update must not contain a result")
		}
	case domain.JobSucceeded, domain.JobFailed, domain.JobCanceled:
	default:
		return errors.New("agent reported an unsupported job status")
	}
	if update.Progress < 0 || update.Progress > 100 {
		return errors.New("job progress must be between 0 and 100")
	}
	if err := validateResultInstanceIDs(update.Result, job.InstanceID); err != nil {
		return err
	}
	return nil
}

func validateResultInstanceIDs(result map[string]any, expectedInstanceID string) error {
	if result == nil {
		return nil
	}
	pending := []any{result}
	for len(pending) > 0 {
		value := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "instance_id" {
					instanceID, ok := child.(string)
					if !ok || instanceID == "" || instanceID != expectedInstanceID {
						return ErrUnauthorized
					}
					continue
				}
				switch child.(type) {
				case map[string]any, []any:
					pending = append(pending, child)
				}
			}
		case []any:
			for _, child := range typed {
				switch child.(type) {
				case map[string]any, []any:
					pending = append(pending, child)
				}
			}
		}
	}
	return nil
}

func validateAgentInstanceID(instanceID string) error {
	if instanceID == "" || len(instanceID) > 64 || uuid.Validate(instanceID) != nil {
		return errors.New("agent message has an invalid instance ID")
	}
	return nil
}

func normalizeAgentObservationTime(value *time.Time, now time.Time) error {
	now = now.UTC()
	if value.IsZero() {
		*value = now
		return nil
	}
	observed := value.UTC()
	if observed.Before(now.Add(-agentObservationPastWindow)) || observed.After(now.Add(agentObservationFutureWindow)) {
		return errors.New("observation timestamp is outside the accepted clock window")
	}
	*value = observed
	return nil
}

func (h *AgentHub) requireNodeInstances(ctx context.Context, nodeID string, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}
	if h.validateInstances == nil {
		return errors.New("instance ownership validator is unavailable")
	}
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client == nil {
		return h.validateInstances(ctx, nodeID, instanceIDs)
	}
	now := time.Now().UTC()
	if h.now != nil {
		now = h.now().UTC()
	}

	// Validation is serialized independently of the WebSocket write lock. A
	// burst of concurrent frames for one new instance therefore produces one
	// ownership query without delaying job dispatch writes behind SQLite.
	client.ownershipMu.Lock()
	defer client.ownershipMu.Unlock()
	if client.ownership == nil {
		client.ownership = make(map[string]time.Time)
	}
	missing := make([]string, 0, len(instanceIDs))
	seen := make(map[string]struct{}, len(instanceIDs))
	for _, instanceID := range instanceIDs {
		if _, duplicate := seen[instanceID]; duplicate {
			continue
		}
		seen[instanceID] = struct{}{}
		if expiresAt, cached := client.ownership[instanceID]; cached && expiresAt.After(now) {
			continue
		}
		delete(client.ownership, instanceID)
		missing = append(missing, instanceID)
	}
	if len(missing) == 0 {
		return nil
	}
	if err := h.validateInstances(ctx, nodeID, missing); err != nil {
		// Unknown and cross-node IDs are never negative-cached: every later
		// attempt still fails closed against the authoritative database.
		return err
	}
	for instanceID, expiresAt := range client.ownership {
		if !expiresAt.After(now) {
			delete(client.ownership, instanceID)
		}
	}
	if len(client.ownership)+len(missing) > maxAgentOwnershipCacheItems {
		// Agent protocol batches are themselves bounded. Clearing here keeps the
		// cache strictly bounded while retaining the freshly verified working set.
		clear(client.ownership)
	}
	cacheLimit := len(missing)
	if cacheLimit > maxAgentOwnershipCacheItems {
		cacheLimit = maxAgentOwnershipCacheItems
	}
	for _, instanceID := range missing[:cacheLimit] {
		client.ownership[instanceID] = now.Add(agentOwnershipCacheTTL)
	}
	return nil
}

func (h *AgentHub) invalidateInstanceOwnership(instanceID string) {
	if instanceID == "" {
		return
	}
	h.mu.RLock()
	connections := make([]*agentConnection, 0, len(h.conns))
	for _, client := range h.conns {
		connections = append(connections, client)
	}
	h.mu.RUnlock()
	for _, client := range connections {
		client.ownershipMu.Lock()
		delete(client.ownership, instanceID)
		client.ownershipMu.Unlock()
	}
}

func (c *agentConnection) acceptJobUpdate(jobID string, update protocol.JobUpdate, now time.Time) bool {
	if isTerminalJobStatus(update.Status) {
		delete(c.jobUpdates, jobID)
		return true
	}
	if c.jobUpdates == nil {
		c.jobUpdates = make(map[string]acceptedJobUpdate)
	}
	previous, exists := c.jobUpdates[jobID]
	if exists {
		if previous.status == update.Status && previous.progress == update.Progress && previous.message == update.Message {
			return false
		}
		if now.Sub(previous.at) < minAgentJobUpdateInterval {
			return false
		}
	} else if len(c.jobUpdates) >= maxTrackedAgentJobUpdates {
		for trackedJobID, tracked := range c.jobUpdates {
			if now.Sub(tracked.at) >= agentJobUpdateTrackingWindow {
				delete(c.jobUpdates, trackedJobID)
			}
		}
		if len(c.jobUpdates) >= maxTrackedAgentJobUpdates {
			return false
		}
	}
	c.jobUpdates[jobID] = acceptedJobUpdate{status: update.Status, progress: update.Progress, message: update.Message, at: now}
	return true
}

func (h *AgentHub) acceptJobUpdate(nodeID, jobID string, update protocol.JobUpdate, now time.Time) bool {
	h.mu.RLock()
	client := h.conns[nodeID]
	h.mu.RUnlock()
	if client == nil {
		return true
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.acceptJobUpdate(jobID, update, now)
}
