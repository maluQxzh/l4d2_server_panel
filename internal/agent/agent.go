package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
	"github.com/l4d2-panel/l4d2panel/internal/version"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	netio "github.com/shirou/gopsutil/v4/net"
	processinfo "github.com/shirou/gopsutil/v4/process"
)

type Agent struct {
	configPath string
	config     *Config
	logger     *log.Logger
	paths      *PathGuard
	journal    *Journal
	registry   *Registry
	processes  *ProcessManager
	executor   *Executor

	ctx            context.Context
	senderMu       sync.RWMutex
	sender         chan protocol.Envelope
	replayWake     chan struct{}
	activeMu       sync.Mutex
	activeJobs     map[string]context.CancelFunc
	pendingCancels map[string]bool
	recoveryOnce   sync.Once
}

func New(configPath string, config *Config, logger *log.Logger) (*Agent, error) {
	if logger == nil {
		logger = log.Default()
	}
	if err := osMkdirData(config); err != nil {
		return nil, err
	}
	paths, err := NewPathGuard(config.InstanceRoots)
	if err != nil {
		return nil, err
	}
	journal, err := OpenJournal(config.DataDir)
	if err != nil {
		return nil, err
	}
	registry, err := OpenRegistry(config.DataDir)
	if err != nil {
		return nil, err
	}
	agent := &Agent{configPath: configPath, config: config, logger: logger, paths: paths, journal: journal, registry: registry, activeJobs: make(map[string]context.CancelFunc), pendingCancels: make(map[string]bool)}
	processes, err := NewProcessManager(config.DataDir, logger, func(instanceID, stream, line string, at time.Time) { agent.emitLog(instanceID, stream, line, at) })
	if err != nil {
		return nil, err
	}
	agent.processes = processes
	agent.executor = NewExecutor(config, paths, registry, processes, logger)
	if agent.executor.mapStoreErr != nil {
		return nil, fmt.Errorf("initialize map library: %w", agent.executor.mapStoreErr)
	}
	return agent, nil
}

func osMkdirData(config *Config) error {
	if err := os.MkdirAll(config.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data_dir: %w", err)
	}
	return nil
}

func (a *Agent) Enroll(ctx context.Context) error { return Enroll(ctx, a.configPath, a.config) }

func (a *Agent) Run(ctx context.Context) error {
	a.ctx = ctx
	a.recoveryOnce.Do(a.recoverInstances)
	go a.runLogCleanup(ctx)
	if a.config.Credential == "" || a.config.NodeID == "" {
		if err := a.Enroll(ctx); err != nil {
			return err
		}
	}
	minDelay, _ := a.config.reconnectMin()
	maxDelay, _ := a.config.reconnectMax()
	delay := minDelay
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := a.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		a.logger.Printf("control connection ended: %v", err)
		jitter := time.Duration(rand.Int64N(max(int64(delay/3), 1)))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay + jitter):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

func (a *Agent) runLogCleanup(ctx context.Context) {
	cleanup := func() {
		removed, err := a.processes.CleanupLogs(time.Now(), 7*24*time.Hour)
		if err != nil {
			a.logger.Printf("instance log retention completed with errors: %v", err)
		}
		if removed > 0 {
			a.logger.Printf("removed %d expired instance log segments", removed)
		}
		removed, err = CleanupBackupUploads(a.config.DataDir, time.Now())
		if err != nil {
			a.logger.Printf("backup upload staging retention completed with errors: %v", err)
		}
		if removed > 0 {
			a.logger.Printf("removed %d expired backup upload staging files", removed)
		}
	}
	cleanup()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func (a *Agent) recoverInstances() {
	for _, record := range a.registry.List() {
		if !record.DesiredRunning || !record.AutoRestart || record.ExternalPID > 0 {
			continue
		}
		instancePath, err := resolveInstanceRecordPath(a.paths, record)
		if err == nil {
			err = ValidateStartArgs(record.Args)
		}
		if err == nil {
			err = ProbeLocalPorts(record.RCONPort, record.TVPort)
		}
		executable := record.Executable
		if executable == "" {
			executable = defaultServerExecutable()
		}
		var executablePath string
		if err == nil {
			executablePath, err = ResolveWithin(instancePath, executable)
		}
		if err == nil {
			_, err = a.processes.Start(ProcessSpec{InstanceID: record.ID, Dir: instancePath, Executable: executablePath, Args: append([]string(nil), record.Args...), AutoRestart: record.AutoRestart})
		}
		if err != nil {
			a.processes.MarkRecoveryFailed(record.ID, err.Error())
			a.logger.Printf("automatic recovery failed for instance %s: %v", record.ID, err)
			continue
		}
		a.logger.Printf("automatically recovered instance %s", record.ID)
	}
}

func (a *Agent) runSession(ctx context.Context) error {
	endpoint, err := a.config.endpoint("/agent/v1/connect", true)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: a.config.InsecureSkipVerify}} //nolint:gosec -- explicit operator setting
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+a.config.Credential)
	conn, response, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if response != nil {
			return fmt.Errorf("websocket rejected (%s): %w", response.Status, err)
		}
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(16 << 20)
	hello, err := protocol.NewEnvelope(protocol.TypeHello, a.config.NodeID, "", protocol.Hello{Name: a.config.NodeName, OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: version.Version, InstanceRoot: a.config.InstanceRoots[0], Capabilities: []string{"absolute_game_paths", "backup_upload", "check_update", "discover_instances", "game_installation_delete_v1", "map_hot_reload_v1", "map_library_v1", "package_delete_v1", "package_scan_v1", "purge_quarantine", "qq_flash_file_v1", "qq_flash_package_v1", "qq_flash_vpk_packages_v2", "runtime_environment_v1", "server_cfg_native"}})
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(hello); err != nil {
		return err
	}
	sender := make(chan protocol.Envelope, 512)
	replayWake := make(chan struct{}, 1)
	a.setSessionSender(sender, replayWake)
	defer a.clearSessionSender(sender)
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writeErrors := make(chan error, 1)
	go func() { writeErrors <- a.writeLoop(sessionCtx, conn, sender); _ = conn.Close() }()
	go a.replayTerminalJournal(sessionCtx, sender, replayWake)
	a.wakeTerminalReplay()
	heartbeatEvery, _ := a.config.heartbeatEvery()
	_ = conn.SetReadDeadline(time.Now().Add(heartbeatEvery * 3))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(heartbeatEvery * 3)) })
	for {
		var envelope protocol.Envelope
		if err := conn.ReadJSON(&envelope); err != nil {
			select {
			case writeErr := <-writeErrors:
				if writeErr != nil {
					return writeErr
				}
			default:
			}
			return err
		}
		if !protocol.CompatibleVersion(envelope.Version) {
			return fmt.Errorf("unsupported protocol version %q", envelope.Version)
		}
		if envelope.NodeID != "" && envelope.NodeID != a.config.NodeID {
			return errors.New("message targets a different node")
		}
		switch envelope.Type {
		case protocol.TypeJob:
			var command protocol.JobCommand
			if err := json.Unmarshal(envelope.Payload, &command); err != nil {
				a.logger.Printf("invalid job envelope: %v", err)
				continue
			}
			if command.Job.ID == "" {
				command.Job.ID = envelope.JobID
			}
			go a.handleJob(command.Job)
		case protocol.TypeJobCancel:
			if envelope.JobID == "" {
				a.logger.Printf("ignoring job cancellation without job_id")
				continue
			}
			a.cancelJob(envelope.JobID)
		case protocol.TypeJobAck:
			if envelope.JobID == "" {
				a.logger.Printf("ignoring job acknowledgement without job_id")
				continue
			}
			if err := a.journal.Acknowledge(envelope.JobID); err != nil {
				a.logger.Printf("persist job acknowledgement for %s: %v", envelope.JobID, err)
			} else {
				// Refill the bounded replay window on this same connection. Without
				// this signal, entries beyond the first page could remain terminal
				// locally while Control keeps their jobs in running forever.
				a.wakeTerminalReplay()
				// Unknown-job cancellations use the same acknowledgement path but
				// have no journal row. Once Control durably accepts that terminal
				// outcome it will never dispatch the job, so the in-memory late-send
				// guard can be released instead of growing for the Agent lifetime.
				a.activeMu.Lock()
				delete(a.pendingCancels, envelope.JobID)
				a.activeMu.Unlock()
			}
		default:
			a.logger.Printf("ignoring unsupported control message %q", envelope.Type)
		}
	}
}

const terminalReplayBatchSize = 200

// replayTerminalJournal keeps a bounded set of durable terminal outcomes in
// flight and refills it after every acknowledgement. It is deliberately a
// separate producer so a large reconnect backlog never blocks heartbeat and
// metric generation in writeLoop. Periodic requeue is a final safety net for a
// lost ACK which did not otherwise tear down the WebSocket.
func (a *Agent) replayTerminalJournal(ctx context.Context, sender chan<- protocol.Envelope, wake <-chan struct{}) {
	inFlight := make(map[string]struct{}, terminalReplayBatchSize)
	retry := time.NewTicker(30 * time.Second)
	defer retry.Stop()

	refill := func(requeue bool) bool {
		if requeue {
			clear(inFlight)
		} else {
			for jobID := range inFlight {
				entry, ok := a.journal.Get(jobID)
				if !ok || !isFinalJobStatus(entry.Status) {
					delete(inFlight, jobID)
				}
			}
		}
		if len(inFlight) >= terminalReplayBatchSize {
			return true
		}
		for _, entry := range a.journal.FinalEntries(terminalReplayBatchSize) {
			if _, exists := inFlight[entry.JobID]; exists {
				continue
			}
			envelope, err := protocol.NewEnvelope(protocol.TypeJobUpdate, a.config.NodeID, entry.JobID,
				protocol.JobUpdate{Status: entry.Status, Progress: entry.Progress, Message: protocol.TruncateJobUpdateMessage(entry.Message), Result: entry.Result})
			if err != nil {
				a.logger.Printf("encode terminal journal replay for %s: %v", entry.JobID, err)
				continue
			}
			select {
			case sender <- envelope:
				inFlight[entry.JobID] = struct{}{}
			case <-ctx.Done():
				return false
			}
			if len(inFlight) >= terminalReplayBatchSize {
				break
			}
		}
		return true
	}

	if !refill(false) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if !refill(false) {
				return
			}
		case <-retry.C:
			if !refill(true) {
				return
			}
		}
	}
}

func (a *Agent) writeLoop(ctx context.Context, conn *websocket.Conn, sender <-chan protocol.Envelope) error {
	heartbeatEvery, _ := a.config.heartbeatEvery()
	metricsEvery, _ := a.config.metricsEvery()
	heartbeats := time.NewTicker(heartbeatEvery)
	metrics := time.NewTicker(metricsEvery)
	defer heartbeats.Stop()
	defer metrics.Stop()
	write := func(envelope protocol.Envelope) error {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(envelope)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case envelope := <-sender:
			if err := write(envelope); err != nil {
				return err
			}
		case <-heartbeats.C:
			envelope, err := protocol.NewEnvelope(protocol.TypeHeartbeat, a.config.NodeID, "", a.heartbeat())
			if err != nil {
				return err
			}
			if err := write(envelope); err != nil {
				return err
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return err
			}
		case <-metrics.C:
			envelope, err := protocol.NewEnvelope(protocol.TypeMetrics, a.config.NodeID, "", a.metrics())
			if err != nil {
				return err
			}
			if err := write(envelope); err != nil {
				return err
			}
		}
	}
}

func (a *Agent) heartbeat() protocol.Heartbeat {
	states := a.processes.States()
	instances := make(map[string]string)
	for _, record := range a.registry.List() {
		state, ok := states[record.ID]
		if ok {
			instances[record.ID] = state.Status
		} else if record.ExternalPID > 0 {
			// External is an ownership state, not only a liveness sample. Keep
			// it until an explicit controlled restart installs the managed
			// configuration and starts a process supervised by the Agent.
			instances[record.ID] = "external"
		} else {
			instances[record.ID] = "stopped"
		}
	}
	return protocol.Heartbeat{AgentVersion: version.Version, Instances: instances}
}

func (a *Agent) metrics() []domain.MetricSample {
	now := time.Now().UTC()
	node := domain.MetricSample{NodeID: a.config.NodeID, CollectedAt: now}
	if values, err := cpu.Percent(0, false); err == nil && len(values) > 0 {
		node.CPUPercent = values[0]
	}
	if value, err := mem.VirtualMemory(); err == nil {
		node.MemoryUsed, node.MemoryTotal = value.Used, value.Total
	}
	if value, err := disk.Usage(a.config.InstanceRoots[0]); err == nil {
		node.DiskUsed, node.DiskTotal = value.Used, value.Total
	}
	if values, err := netio.IOCounters(false); err == nil && len(values) > 0 {
		node.NetworkRx, node.NetworkTx = values[0].BytesRecv, values[0].BytesSent
	}
	result := []domain.MetricSample{node}
	for instanceID, state := range a.processes.States() {
		if state.PID <= 0 {
			continue
		}
		sample := domain.MetricSample{NodeID: a.config.NodeID, InstanceID: instanceID, CollectedAt: now}
		sample.Players = a.executor.cachedPlayers(instanceID, now)
		if process, err := processinfo.NewProcess(int32(state.PID)); err == nil {
			if value, err := process.CPUPercent(); err == nil {
				sample.CPUPercent = value
			}
			if value, err := process.MemoryInfo(); err == nil {
				sample.MemoryUsed = value.RSS
			}
		}
		result = append(result, sample)
	}
	return result
}

func (a *Agent) handleJob(job domain.Job) {
	if job.NodeID != "" && job.NodeID != a.config.NodeID {
		a.logger.Printf("rejecting job %s for another node", job.ID)
		return
	}
	existing, err := a.journal.Begin(job)
	if err != nil {
		a.sendJobUpdate(job.ID, domain.JobFailed, 0, err.Error(), nil)
		return
	}
	if existing != nil {
		a.sendJobUpdate(job.ID, existing.Status, existing.Progress, existing.Message, existing.Result)
		return
	}
	a.handleClaimedJob(job, a.executor.Execute)
}

type executeJobFunc func(context.Context, domain.Job, ProgressFunc) (map[string]any, error)

// handleClaimedJob runs a job that has already been durably claimed in the
// journal. Keeping the executor as an argument makes the commit/cancel race
// deterministic in tests without weakening the production Executor type.
func (a *Agent) handleClaimedJob(job domain.Job, execute executeJobFunc) {
	jobCtx, cancel := context.WithCancel(a.ctx)
	a.activeMu.Lock()
	a.activeJobs[job.ID] = cancel
	canceledBeforeStart := a.pendingCancels[job.ID]
	if canceledBeforeStart {
		delete(a.pendingCancels, job.ID)
		cancel()
	}
	a.activeMu.Unlock()
	defer func() { cancel(); a.activeMu.Lock(); delete(a.activeJobs, job.ID); a.activeMu.Unlock() }()
	if canceledBeforeStart {
		const message = "operation canceled before execution"
		_ = a.journal.Update(job.ID, domain.JobCanceled, 0, message, nil)
		a.sendJobUpdate(job.ID, domain.JobCanceled, 0, message, nil)
		return
	}
	_ = a.journal.Update(job.ID, domain.JobRunning, 0, "operation started", nil)
	a.sendJobUpdate(job.ID, domain.JobRunning, 0, "operation started", nil)
	progress := newJournalProgress(a, job.ID)
	result, executeErr := execute(jobCtx, job, progress.Update)
	status, message := executionCompletion(executeErr)
	if status == domain.JobSucceeded {
		_ = a.journal.Update(job.ID, status, 100, message, result)
		a.sendJobUpdate(job.ID, status, 100, message, result)
		return
	}
	_ = a.journal.Update(job.ID, status, progress.Value(), message, nil)
	a.sendJobUpdate(job.ID, status, progress.Value(), message, nil)
}

// executionCompletion deliberately classifies the Executor's return value,
// not the context's state. A cancellation can arrive after an operation has
// committed and returned success; in that case the durable result must remain
// succeeded. Only an explicit cancellation returned by Execute is a canceled
// job. Operation-specific deadlines are failures: treating them as user
// cancellations hides the actual timeout and makes the task center misleading.
func executionCompletion(executeErr error) (domain.JobStatus, string) {
	if executeErr == nil {
		return domain.JobSucceeded, "operation completed"
	}
	if errors.Is(executeErr, context.Canceled) {
		return domain.JobCanceled, "operation canceled"
	}
	return domain.JobFailed, sanitizeJobError(executeErr)
}

func (a *Agent) cancelJob(jobID string) {
	a.activeMu.Lock()
	if cancel := a.activeJobs[jobID]; cancel != nil {
		cancel()
		a.activeMu.Unlock()
		return
	}
	a.activeMu.Unlock()
	entry, ok := a.journal.Get(jobID)
	if ok && isFinalJobStatus(entry.Status) {
		a.sendJobUpdate(jobID, entry.Status, entry.Progress, entry.Message, entry.Result)
		return
	}
	a.activeMu.Lock()
	if cancel := a.activeJobs[jobID]; cancel != nil {
		cancel()
	} else {
		a.pendingCancels[jobID] = true
	}
	a.activeMu.Unlock()
	if !ok {
		a.sendJobUpdate(jobID, domain.JobCanceled, 0, "operation canceled before dispatch", nil)
	}
}

func (a *Agent) sendJobUpdate(jobID string, status domain.JobStatus, progress int, message string, result map[string]any) {
	message = protocol.TruncateJobUpdateMessage(message)
	if isFinalJobStatus(status) {
		if entry, ok := a.journal.Get(jobID); ok && isFinalJobStatus(entry.Status) {
			// Terminal outcomes are sent only by the reliable journal replay
			// producer. Ordinary best-effort queue saturation must never strand a
			// Control-plane job in running.
			if a.wakeTerminalReplay() {
				return
			}
			// A few local integrations embed Agent without a live session replay
			// producer. Preserve their direct sender behavior; production sessions
			// always install replayWake together with sender.
		}
	}
	envelope, err := protocol.NewEnvelope(protocol.TypeJobUpdate, a.config.NodeID, jobID, protocol.JobUpdate{Status: status, Progress: progress, Message: message, Result: result})
	if err != nil {
		return
	}
	a.send(envelope)
}

func (a *Agent) emitLog(instanceID, stream, line string, at time.Time) {
	if len(line) > 16*1024 {
		line = line[:16*1024]
	}
	envelope, err := protocol.NewEnvelope(protocol.TypeLog, a.config.NodeID, "", protocol.LogEvent{InstanceID: instanceID, Stream: stream, Line: line, At: at})
	if err == nil {
		a.send(envelope)
	}
}

func (a *Agent) send(envelope protocol.Envelope) bool {
	a.senderMu.RLock()
	sender := a.sender
	a.senderMu.RUnlock()
	if sender == nil {
		return false
	}
	select {
	case sender <- envelope:
		return true
	default:
		return false
	}
}

func (a *Agent) setSessionSender(sender chan protocol.Envelope, replayWake chan struct{}) {
	a.senderMu.Lock()
	a.sender = sender
	a.replayWake = replayWake
	a.senderMu.Unlock()
}
func (a *Agent) clearSessionSender(sender chan protocol.Envelope) {
	a.senderMu.Lock()
	if a.sender == sender {
		a.sender = nil
		a.replayWake = nil
	}
	a.senderMu.Unlock()
}

func (a *Agent) wakeTerminalReplay() bool {
	a.senderMu.RLock()
	wake := a.replayWake
	a.senderMu.RUnlock()
	if wake == nil {
		return false
	}
	select {
	case wake <- struct{}{}:
	default:
	}
	return true
}

type journalProgress struct {
	agent *Agent
	jobID string
	mu    sync.Mutex
	value int
	last  time.Time
}

func newJournalProgress(agent *Agent, jobID string) *journalProgress {
	return &journalProgress{agent: agent, jobID: jobID}
}
func (p *journalProgress) Update(value int, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value < p.value {
		value = p.value
	}
	if value > 99 {
		value = 99
	}
	if value == p.value && time.Since(p.last) < time.Second {
		return
	}
	p.value, p.last = value, time.Now()
	_ = p.agent.journal.Update(p.jobID, domain.JobRunning, value, message, nil)
	p.agent.sendJobUpdate(p.jobID, domain.JobRunning, value, message, nil)
}
func (p *journalProgress) Value() int { p.mu.Lock(); defer p.mu.Unlock(); return p.value }

func sanitizeJobError(err error) string {
	return protocol.TruncateJobUpdateMessage(redactEmbeddedURLSecrets(err.Error()))
}
