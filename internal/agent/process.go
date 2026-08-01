package agent

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProcessSpec struct {
	InstanceID  string
	Dir         string
	Executable  string
	Args        []string
	AutoRestart bool
}

func sanitizedProcessEnvironment(instanceDir string) []string {
	allowed := map[string]bool{
		"comspec": true, "lang": true, "lc_all": true, "path": true,
		"pathext": true, "systemroot": true, "temp": true, "tmp": true,
		"tmpdir": true, "tz": true, "windir": true,
	}
	values := make(map[string]string)
	for _, item := range os.Environ() {
		name, _, ok := strings.Cut(item, "=")
		if !ok || !allowed[strings.ToLower(name)] {
			continue
		}
		values[strings.ToLower(name)] = item
	}
	// Keep game-created per-user files inside the instance instead of the
	// Agent service profile. In particular, no enrollment or control-plane
	// environment variables are inherited by srcds or native extensions.
	values["home"] = "HOME=" + instanceDir
	if runtime.GOOS == "windows" {
		values["userprofile"] = "USERPROFILE=" + instanceDir
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, values[key])
	}
	return result
}

type ProcessState struct {
	Status    string    `json:"status"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	ExitError string    `json:"exit_error,omitempty"`
}

type managedProcess struct {
	spec          ProcessSpec
	cmd           *exec.Cmd
	platform      *platformProcess
	state         ProcessState
	done          chan struct{}
	stopRequested bool
	launching     bool
	crashes       []time.Time
}

type ProcessManager struct {
	mu     sync.RWMutex
	items  map[string]*managedProcess
	logDir string
	logger *log.Logger
	onLine func(instanceID, stream, line string, at time.Time)
}

func NewProcessManager(dataDir string, logger *log.Logger, onLine func(string, string, string, time.Time)) (*ProcessManager, error) {
	dir := filepath.Join(dataDir, "logs", "instances")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &ProcessManager{items: make(map[string]*managedProcess), logDir: dir, logger: logger, onLine: onLine}, nil
}

func (m *ProcessManager) Start(spec ProcessSpec) (ProcessState, error) {
	if spec.InstanceID == "" {
		return ProcessState{}, errors.New("instance id is required")
	}
	if len(spec.Args) > 256 {
		return ProcessState{}, errors.New("too many process arguments")
	}
	m.mu.Lock()
	if old := m.items[spec.InstanceID]; old != nil && (old.launching || old.cmd != nil) {
		state := old.state
		m.mu.Unlock()
		return state, errors.New("instance process is already running")
	}
	p := &managedProcess{spec: spec, state: ProcessState{Status: "starting"}, done: make(chan struct{}), launching: true}
	m.items[spec.InstanceID] = p
	m.mu.Unlock()
	if err := m.launch(p); err != nil {
		m.mu.Lock()
		p.launching = false
		p.state.Status = "stopped"
		p.state.ExitError = err.Error()
		close(p.done)
		m.mu.Unlock()
		return p.state, err
	}
	m.mu.RLock()
	state := p.state
	m.mu.RUnlock()
	return state, nil
}

func (m *ProcessManager) launch(p *managedProcess) error {
	if st, err := os.Stat(p.spec.Executable); err != nil || st.IsDir() {
		if err == nil {
			err = errors.New("executable is a directory")
		}
		return fmt.Errorf("executable unavailable: %w", err)
	}
	instanceLog, err := NewRotatingLogWithRetention(filepath.Join(m.logDir, safeLogName(p.spec.InstanceID)+".log"), 100<<20, 19, 7*24*time.Hour)
	if err != nil {
		return err
	}
	stdout := &lineSink{stream: "stdout", log: instanceLog, emit: func(stream, line string, at time.Time) {
		if m.onLine != nil {
			m.onLine(p.spec.InstanceID, stream, line, at)
		}
	}}
	stderr := &lineSink{stream: "stderr", log: instanceLog, emit: func(stream, line string, at time.Time) {
		if m.onLine != nil {
			m.onLine(p.spec.InstanceID, stream, line, at)
		}
	}}
	cmd := exec.Command(p.spec.Executable, p.spec.Args...)
	cmd.Dir = p.spec.Dir
	cmd.Env = sanitizedProcessEnvironment(p.spec.Dir)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = stdout, stderr
	preparePlatformCommand(cmd)
	if err := cmd.Start(); err != nil {
		_ = instanceLog.Close()
		return err
	}
	platform, err := attachPlatformProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = instanceLog.Close()
		return fmt.Errorf("attach process tree: %w", err)
	}
	m.mu.Lock()
	if p.stopRequested {
		m.mu.Unlock()
		_ = forcePlatformProcess(platform, cmd.Process)
		closePlatformProcess(platform)
		_ = instanceLog.Close()
		return errors.New("start was canceled")
	}
	p.cmd, p.platform, p.launching = cmd, platform, false
	p.state = ProcessState{Status: "running", PID: cmd.Process.Pid, StartedAt: time.Now().UTC()}
	m.mu.Unlock()
	go m.wait(p, cmd, platform, instanceLog, stdout, stderr)
	return nil
}

func (m *ProcessManager) wait(p *managedProcess, cmd *exec.Cmd, platform *platformProcess, instanceLog io.Closer, sinks ...*lineSink) {
	err := cmd.Wait()
	for _, sink := range sinks {
		sink.Flush()
	}
	closePlatformProcess(platform)
	_ = instanceLog.Close()
	now := time.Now().UTC()
	m.mu.Lock()
	if p.cmd != cmd {
		m.mu.Unlock()
		return
	}
	p.cmd, p.platform = nil, nil
	p.state.Status, p.state.PID = "stopped", 0
	if err != nil {
		p.state.ExitError = err.Error()
	} else {
		p.state.ExitError = ""
	}
	close(p.done)
	restart := p.spec.AutoRestart && !p.stopRequested
	if restart {
		cutoff := now.Add(-10 * time.Minute)
		kept := p.crashes[:0]
		for _, at := range p.crashes {
			if at.After(cutoff) {
				kept = append(kept, at)
			}
		}
		p.crashes = append(kept, now)
		if len(p.crashes) > 5 {
			restart = false
			p.state.Status = "crash_loop"
		}
	}
	m.mu.Unlock()
	if restart {
		m.logger.Printf("instance %s exited unexpectedly (%v); restarting", p.spec.InstanceID, err)
		time.AfterFunc(5*time.Second, func() {
			m.mu.Lock()
			if p.stopRequested || p.cmd != nil || m.items[p.spec.InstanceID] != p {
				m.mu.Unlock()
				return
			}
			p.done = make(chan struct{})
			p.launching = true
			p.state.Status = "starting"
			m.mu.Unlock()
			if launchErr := m.launch(p); launchErr != nil {
				m.mu.Lock()
				p.launching = false
				p.state.Status = "crash_loop"
				p.state.ExitError = launchErr.Error()
				close(p.done)
				m.mu.Unlock()
			}
		})
	}
}

func (m *ProcessManager) Stop(instanceID string, timeout time.Duration) error {
	m.mu.Lock()
	p := m.items[instanceID]
	if p == nil || (p.cmd == nil && !p.launching) {
		m.mu.Unlock()
		return nil
	}
	p.stopRequested = true
	cmd, platform, done := p.cmd, p.platform, p.done
	if cmd != nil {
		p.state.Status = "stopping"
	}
	m.mu.Unlock()
	if cmd == nil {
		select {
		case <-done:
			return nil
		case <-time.After(timeout):
			return errors.New("timed out waiting for process launch")
		}
	}
	_ = gracefulPlatformProcess(platform, cmd.Process)
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		if err := forcePlatformProcess(platform, cmd.Process); err != nil {
			return err
		}
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("process did not exit after force termination")
		}
	}
}

func (m *ProcessManager) State(instanceID string) ProcessState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p := m.items[instanceID]; p != nil {
		return p.state
	}
	return ProcessState{Status: "stopped"}
}

// HasActiveProcess reports process ownership rather than health. A degraded
// state can either describe a live process that failed health checks or a
// stopped recovery-failure marker; only the former must block offline file
// operations such as restoring a backup.
func (m *ProcessManager) HasActiveProcess(instanceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.items[instanceID]
	return p != nil && (p.cmd != nil || p.launching)
}

func (m *ProcessManager) States() map[string]ProcessState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]ProcessState, len(m.items))
	for id, p := range m.items {
		result[id] = p.state
	}
	return result
}

func (m *ProcessManager) MarkDegraded(instanceID, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.items[instanceID]; p != nil && p.cmd != nil {
		p.state.Status = "degraded"
		p.state.ExitError = message
	}
}

func (m *ProcessManager) MarkRecoveryFailed(instanceID, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.items[instanceID]; current != nil && (current.cmd != nil || current.launching) {
		return
	}
	done := make(chan struct{})
	close(done)
	m.items[instanceID] = &managedProcess{state: ProcessState{Status: "degraded", ExitError: message}, done: done}
}

func (m *ProcessManager) LogTail(instanceID string, lines int) ([]string, error) {
	if lines <= 0 || lines > 5000 {
		lines = 200
	}
	return tailLines(filepath.Join(m.logDir, safeLogName(instanceID)+".log"), lines)
}

func safeLogName(id string) string {
	result := make([]rune, 0, len(id))
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			result = append(result, r)
		}
	}
	if len(result) == 0 {
		return "unknown"
	}
	return string(result)
}
