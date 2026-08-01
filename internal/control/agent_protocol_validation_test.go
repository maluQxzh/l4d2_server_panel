package control

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestAgentMessagesRejectInstancesOutsideConnectedNode(t *testing.T) {
	nodeID := uuid.NewString()
	instanceID := uuid.NewString()
	now := time.Now().UTC()
	tests := []struct {
		name     string
		envelope protocol.Envelope
	}{
		{
			name:     "heartbeat",
			envelope: mustAgentEnvelope(t, protocol.TypeHeartbeat, nodeID, "", protocol.Heartbeat{AgentVersion: "test", Instances: map[string]string{instanceID: "running"}}),
		},
		{
			name:     "metrics",
			envelope: mustAgentEnvelope(t, protocol.TypeMetrics, nodeID, "", []domain.MetricSample{{InstanceID: instanceID, CPUPercent: 12, CollectedAt: now}}),
		},
		{
			name:     "log",
			envelope: mustAgentEnvelope(t, protocol.TypeLog, nodeID, "", protocol.LogEvent{InstanceID: instanceID, Stream: "stdout", Line: "ready", At: now}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := NewAgentHub(nil, nil, slog.Default(), nil)
			calls := 0
			hub.validateInstances = func(_ context.Context, gotNodeID string, gotInstanceIDs []string) error {
				calls++
				if gotNodeID != nodeID || len(gotInstanceIDs) != 1 || gotInstanceIDs[0] != instanceID {
					t.Fatalf("unexpected ownership lookup: node=%q instances=%v", gotNodeID, gotInstanceIDs)
				}
				return ErrUnauthorized
			}
			err := hub.handleEnvelope(context.Background(), domain.Node{ID: nodeID}, test.envelope)
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("cross-node %s was not rejected: %v", test.name, err)
			}
			if calls != 1 {
				t.Fatalf("ownership validator called %d times", calls)
			}
		})
	}
}

func TestAgentOwnershipCacheIsConcurrentBoundedAndInvalidatable(t *testing.T) {
	nodeID := uuid.NewString()
	instanceID := uuid.NewString()
	now := time.Now().UTC()
	hub := NewAgentHub(nil, nil, slog.Default(), nil)
	hub.now = func() time.Time { return now }
	hub.conns[nodeID] = &agentConnection{ownership: make(map[string]time.Time)}
	calls := 0
	expectedInstanceID := instanceID
	hub.validateInstances = func(_ context.Context, gotNodeID string, gotInstanceIDs []string) error {
		calls++
		if gotNodeID != nodeID || len(gotInstanceIDs) != 1 || gotInstanceIDs[0] != expectedInstanceID {
			t.Fatalf("unexpected ownership query: node=%q instances=%v", gotNodeID, gotInstanceIDs)
		}
		return nil
	}

	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := hub.requireNodeInstances(context.Background(), nodeID, []string{instanceID}); err != nil {
				t.Errorf("cached ownership validation failed: %v", err)
			}
		}()
	}
	wait.Wait()
	if calls != 1 {
		t.Fatalf("64 concurrent frames issued %d ownership queries, want 1", calls)
	}

	hub.invalidateInstanceOwnership(instanceID)
	if err := hub.requireNodeInstances(context.Background(), nodeID, []string{instanceID}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("explicit invalidation did not force a query: got %d", calls)
	}
	now = now.Add(agentOwnershipCacheTTL + time.Millisecond)
	if err := hub.requireNodeInstances(context.Background(), nodeID, []string{instanceID}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("TTL expiry did not force a query: got %d", calls)
	}

	client := hub.conns[nodeID]
	client.ownershipMu.Lock()
	for index := 0; index < maxAgentOwnershipCacheItems+100; index++ {
		client.ownership[uuid.NewString()] = now.Add(time.Minute)
	}
	client.ownershipMu.Unlock()
	boundedInstanceID := uuid.NewString()
	expectedInstanceID = boundedInstanceID
	if err := hub.requireNodeInstances(context.Background(), nodeID, []string{boundedInstanceID}); err != nil {
		t.Fatal(err)
	}
	client.ownershipMu.Lock()
	cacheItems := len(client.ownership)
	client.ownershipMu.Unlock()
	if cacheItems > maxAgentOwnershipCacheItems {
		t.Fatalf("ownership cache retained %d entries, maximum is %d", cacheItems, maxAgentOwnershipCacheItems)
	}
}

func TestAgentOwnershipCacheNeverCachesRejectedOrCrossNodeIDs(t *testing.T) {
	nodeA, nodeB := uuid.NewString(), uuid.NewString()
	instanceID := uuid.NewString()
	hub := NewAgentHub(nil, nil, slog.Default(), nil)
	hub.conns[nodeA] = &agentConnection{ownership: make(map[string]time.Time)}
	hub.conns[nodeB] = &agentConnection{ownership: make(map[string]time.Time)}
	calls := 0
	hub.validateInstances = func(_ context.Context, nodeID string, _ []string) error {
		calls++
		if nodeID == nodeA {
			return nil
		}
		return ErrUnauthorized
	}
	if err := hub.requireNodeInstances(context.Background(), nodeA, []string{instanceID}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := hub.requireNodeInstances(context.Background(), nodeB, []string{instanceID}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("cross-node ownership was accepted: %v", err)
		}
	}
	if calls != 3 {
		t.Fatalf("rejected IDs were cached: validator called %d times, want 3", calls)
	}
}

func TestAgentLogRateDoesNotMultiplyOwnershipQueries(t *testing.T) {
	nodeID, instanceID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	hub := NewAgentHub(nil, NewEventBroker(nil), slog.Default(), nil)
	hub.now = func() time.Time { return now }
	hub.conns[nodeID] = &agentConnection{ownership: make(map[string]time.Time)}
	calls := 0
	hub.validateInstances = func(_ context.Context, _ string, _ []string) error {
		calls++
		return nil
	}
	envelope := mustAgentEnvelope(t, protocol.TypeLog, nodeID, "", protocol.LogEvent{
		InstanceID: instanceID, Stream: "stdout", Line: "ready", At: now,
	})
	for range 1_000 {
		if err := hub.handleEnvelope(context.Background(), domain.Node{ID: nodeID}, envelope); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("1,000 log frames issued %d ownership queries, want 1", calls)
	}
}

func TestValidateHeartbeatPayload(t *testing.T) {
	instanceID := uuid.NewString()
	heartbeat := protocol.Heartbeat{AgentVersion: "1.2.3", Instances: map[string]string{instanceID: " Crash-Loop "}}
	ids, err := validateHeartbeatPayload(&heartbeat, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != instanceID || heartbeat.Instances[instanceID] != "crash-loop" {
		t.Fatalf("heartbeat was not normalized: ids=%v heartbeat=%#v", ids, heartbeat)
	}

	heartbeat.Instances[instanceID] = "arbitrary"
	if _, err := validateHeartbeatPayload(&heartbeat, 128); err == nil {
		t.Fatal("unsupported instance status was accepted")
	}
	if _, err := validateHeartbeatPayload(&heartbeat, maxAgentHeartbeatPayload+1); err == nil {
		t.Fatal("oversized heartbeat was accepted")
	}

	many := protocol.Heartbeat{Instances: make(map[string]string, maxAgentHeartbeatInstances+1)}
	for range maxAgentHeartbeatInstances + 1 {
		many.Instances[uuid.NewString()] = "stopped"
	}
	if _, err := validateHeartbeatPayload(&many, 1); err == nil {
		t.Fatal("heartbeat instance limit was not enforced")
	}
}

func TestValidateMetricSamplesBoundsAndClock(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	instanceID := uuid.NewString()
	samples := []domain.MetricSample{
		{CPUPercent: 10, MemoryUsed: 4, MemoryTotal: 8, DiskUsed: 5, DiskTotal: 10, CollectedAt: now},
		{InstanceID: instanceID, CPUPercent: 150, MemoryUsed: 1024},
	}
	ids, err := validateMetricSamples(samples, 256, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != instanceID || !samples[1].CollectedAt.Equal(now) {
		t.Fatalf("metrics were not normalized: ids=%v samples=%#v", ids, samples)
	}

	invalid := []struct {
		name   string
		sample domain.MetricSample
	}{
		{name: "cpu", sample: domain.MetricSample{CPUPercent: maxAgentMetricCPUPercent + 1, CollectedAt: now}},
		{name: "memory", sample: domain.MetricSample{MemoryUsed: 2, MemoryTotal: 1, CollectedAt: now}},
		{name: "disk", sample: domain.MetricSample{DiskUsed: 2, DiskTotal: 1, CollectedAt: now}},
		{name: "players", sample: domain.MetricSample{Players: maxAgentMetricPlayers + 1, CollectedAt: now}},
		{name: "old timestamp", sample: domain.MetricSample{CollectedAt: now.Add(-agentObservationPastWindow - time.Second)}},
		{name: "future timestamp", sample: domain.MetricSample{CollectedAt: now.Add(agentObservationFutureWindow + time.Second)}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateMetricSamples([]domain.MetricSample{test.sample}, 100, now); err == nil {
				t.Fatalf("invalid metric was accepted: %#v", test.sample)
			}
		})
	}
	duplicate := []domain.MetricSample{{InstanceID: instanceID, CollectedAt: now}, {InstanceID: instanceID, CollectedAt: now}}
	if _, err := validateMetricSamples(duplicate, 100, now); err == nil {
		t.Fatal("duplicate instance metrics were accepted")
	}
	if _, err := validateMetricSamples(nil, 0, now); err == nil {
		t.Fatal("empty metrics batch was accepted")
	}
	if _, err := validateMetricSamples(samples, maxAgentMetricsPayload+1, now); err == nil {
		t.Fatal("oversized metrics payload was accepted")
	}
}

func TestValidateLogEventLimits(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	event := protocol.LogEvent{InstanceID: uuid.NewString(), Stream: " STDERR ", Line: "server ready"}
	ids, err := validateLogEvent(&event, 200, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != event.InstanceID || event.Stream != "stderr" || !event.At.Equal(now) {
		t.Fatalf("log event was not normalized: %#v", event)
	}

	event.Stream = "rcon"
	if _, err := validateLogEvent(&event, 200, now); err == nil {
		t.Fatal("unsupported log stream was accepted")
	}
	event.Stream = "stdout"
	event.Line = strings.Repeat("x", maxAgentLogLineBytes+1)
	if _, err := validateLogEvent(&event, 200, now); err == nil {
		t.Fatal("oversized log line was accepted")
	}
	event.Line = "ready"
	event.At = now.Add(-agentObservationPastWindow - time.Second)
	if _, err := validateLogEvent(&event, 200, now); err == nil {
		t.Fatal("stale log event was accepted")
	}
	if _, err := validateLogEvent(&event, maxAgentLogPayload+1, now); err == nil {
		t.Fatal("oversized log payload was accepted")
	}
}

func TestValidateJobUpdateRejectsInjectedResultsAndUnboundedProgress(t *testing.T) {
	nodeID := uuid.NewString()
	instanceID := uuid.NewString()
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID}
	envelope := mustAgentEnvelope(t, protocol.TypeJobUpdate, nodeID, job.ID, protocol.JobUpdate{Status: domain.JobRunning, Progress: 10, Message: "working"})
	update := protocol.JobUpdate{Status: domain.JobRunning, Progress: 10, Message: "working"}
	if err := validateJobUpdate(job, nodeID, envelope, update); err != nil {
		t.Fatalf("valid running update was rejected: %v", err)
	}

	update.Result = map[string]any{}
	if err := validateJobUpdate(job, nodeID, envelope, update); err == nil {
		t.Fatal("running update carrying a result was accepted")
	}
	update.Result = nil
	update.Message = strings.Repeat("x", maxAgentJobMessageBytes+1)
	if err := validateJobUpdate(job, nodeID, envelope, update); err == nil {
		t.Fatal("oversized progress message was accepted")
	}
	update = protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100, Result: map[string]any{"instance_id": uuid.NewString()}}
	if !errors.Is(validateJobUpdate(job, nodeID, envelope, update), ErrUnauthorized) {
		t.Fatal("terminal result for another instance was accepted")
	}
	update.Result["instance_id"] = instanceID
	update.Result = map[string]any{"nested": []any{map[string]any{"instance_id": uuid.NewString()}}}
	if !errors.Is(validateJobUpdate(job, nodeID, envelope, update), ErrUnauthorized) {
		t.Fatal("nested terminal result for another instance was accepted")
	}
	update.Result = map[string]any{"instance_id": instanceID}
	if err := validateJobUpdate(job, uuid.NewString(), envelope, update); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("job update from another node was not rejected: %v", err)
	}
	update.Result = nil
	envelope.Payload = make([]byte, maxAgentTerminalJobPayload+1)
	if err := validateJobUpdate(job, nodeID, envelope, update); err == nil {
		t.Fatal("oversized terminal result was accepted")
	}
}

func TestAgentConnectionBoundsRunningJobUpdateFrequency(t *testing.T) {
	connection := &agentConnection{}
	jobID := uuid.NewString()
	now := time.Now().UTC()
	first := protocol.JobUpdate{Status: domain.JobRunning, Progress: 1, Message: "starting"}
	if !connection.acceptJobUpdate(jobID, first, now) {
		t.Fatal("first running update was rejected")
	}
	if connection.acceptJobUpdate(jobID, first, now.Add(time.Second)) {
		t.Fatal("duplicate running update was accepted")
	}
	changed := protocol.JobUpdate{Status: domain.JobRunning, Progress: 2, Message: "downloading"}
	if connection.acceptJobUpdate(jobID, changed, now.Add(minAgentJobUpdateInterval-time.Millisecond)) {
		t.Fatal("running update burst was not coalesced")
	}
	if !connection.acceptJobUpdate(jobID, changed, now.Add(minAgentJobUpdateInterval)) {
		t.Fatal("bounded running update was rejected after interval")
	}
	terminal := protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100}
	if !connection.acceptJobUpdate(jobID, terminal, now.Add(minAgentJobUpdateInterval+time.Millisecond)) {
		t.Fatal("terminal update must never be throttled")
	}
	if len(connection.jobUpdates) != 0 {
		t.Fatal("terminal update did not release throttle state")
	}

	for range maxTrackedAgentJobUpdates {
		if !connection.acceptJobUpdate(uuid.NewString(), first, now) {
			t.Fatal("tracking state reached its limit too early")
		}
	}
	if connection.acceptJobUpdate(uuid.NewString(), first, now) {
		t.Fatal("tracking state grew beyond its hard limit")
	}
	if !connection.acceptJobUpdate(uuid.NewString(), first, now.Add(agentJobUpdateTrackingWindow)) {
		t.Fatal("expired tracking state was not reclaimed")
	}
}

func mustAgentEnvelope(t *testing.T, messageType, nodeID, jobID string, payload any) protocol.Envelope {
	t.Helper()
	envelope, err := protocol.NewEnvelope(messageType, nodeID, jobID, payload)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
