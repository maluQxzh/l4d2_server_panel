package portableupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v4/disk"
)

var (
	ErrUnsupported         = errors.New("portable updates are not supported by this deployment")
	ErrUpdateActive        = errors.New("another portable update is already active")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different package")
)

type State struct {
	JobID            string     `json:"job_id"`
	Status           string     `json:"status"`
	Phase            string     `json:"phase"`
	CurrentVersion   string     `json:"current_version"`
	TargetVersion    string     `json:"target_version,omitempty"`
	PackageSHA256    string     `json:"package_sha256,omitempty"`
	UploadBytes      int64      `json:"upload_bytes,omitempty"`
	Rollback         string     `json:"rollback,omitempty"`
	Error            string     `json:"error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	IdempotentReplay bool       `json:"idempotent_replay,omitempty"`
	Reconciled       bool       `json:"reconciled,omitempty"`
}

type Overview struct {
	Supported      bool    `json:"supported"`
	CurrentVersion string  `json:"current_version"`
	Active         *State  `json:"active,omitempty"`
	History        []State `json:"history"`
	Reason         string  `json:"reason,omitempty"`
}

type Manager struct {
	Root           string
	ConfigPath     string
	DatabasePath   string
	CurrentVersion string
	ExecutablePath string
	ServiceMode    bool
	command        func(string, ...string) error
	ready          func(time.Duration) error
	startWorker    func(string, string, string) error
	mu             sync.Mutex
}

func Detect(configPath, databasePath, currentVersion string, serviceMode bool) *Manager {
	executable, _ := os.Executable()
	executable, _ = filepath.Abs(executable)
	root := filepath.Dir(executable)
	return &Manager{
		Root: root, ConfigPath: configPath, DatabasePath: databasePath,
		CurrentVersion: currentVersion, ExecutablePath: executable, ServiceMode: serviceMode,
	}
}

func (m *Manager) Supported() (bool, string) {
	if !RuntimeSupported() {
		return false, "中心端升级仅支持 Windows x64 便携服务"
	}
	if !m.ServiceMode {
		return false, "前台运行模式不支持中心端一键升级"
	}
	if strings.Contains(strings.ToLower(m.CurrentVersion), "dev") {
		return false, "开发构建不支持中心端一键升级"
	}
	expectedConfig := filepath.Join(m.Root, "data", "config", "control.json")
	if !samePath(m.ConfigPath, expectedConfig) || !samePath(m.DatabasePath, filepath.Join(m.Root, "data", "database", "l4d2panel.sqlite3")) {
		return false, "当前进程不是 Windows 便携部署"
	}
	if info, err := os.Stat(filepath.Join(m.Root, ManifestName)); err != nil || !info.Mode().IsRegular() {
		return false, "便携部署清单不存在"
	}
	return true, ""
}

func (m *Manager) Overview() Overview {
	overview := Overview{CurrentVersion: m.CurrentVersion, History: []State{}}
	overview.Supported, overview.Reason = m.Supported()
	if !overview.Supported {
		return overview
	}
	overview.Active, _ = m.activeState()
	states, _ := m.listStates()
	for _, state := range states {
		if state.Status == "queued" || state.Status == "running" {
			continue
		}
		overview.History = append(overview.History, state)
		if len(overview.History) == 10 {
			break
		}
	}
	return overview
}

func (m *Manager) State(jobID string) (State, error) {
	if _, err := uuid.Parse(jobID); err != nil {
		return State{}, os.ErrNotExist
	}
	return readState(m.statePath(jobID))
}

func (m *Manager) PendingReconciliation() []State {
	if supported, _ := m.Supported(); !supported {
		return nil
	}
	states, _ := m.listStates()
	result := make([]State, 0)
	for _, state := range states {
		if !state.Reconciled && (state.Status == "succeeded" || state.Status == "failed") {
			result = append(result, state)
		}
	}
	return result
}

func (m *Manager) MarkReconciled(jobID string) error {
	return m.updateState(jobID, "", "", func(state *State) { state.Reconciled = true })
}

func (m *Manager) Create(reader io.Reader, idempotencyKey string) (State, error) {
	if supported, _ := m.Supported(); !supported {
		return State{}, ErrUnsupported
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		return State{}, errors.New("Idempotency-Key is required and must not exceed 200 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.incomingDir(), 0o700); err != nil {
		return State{}, err
	}
	temp, err := os.CreateTemp(m.incomingDir(), "upload-*.zip")
	if err != nil {
		return State{}, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(reader, MaxPackageBytes+1))
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil {
		if copyErr != nil {
			return State{}, copyErr
		}
		return State{}, closeErr
	}
	if written <= 0 || written > MaxPackageBytes {
		return State{}, fmt.Errorf("update package must be no larger than %d bytes", MaxPackageBytes)
	}
	packageHash := hex.EncodeToString(hash.Sum(nil))
	if replay, found, err := m.lookupIdempotency(idempotencyKey); err != nil {
		return State{}, err
	} else if found {
		if replay.PackageSHA256 != packageHash {
			return State{}, ErrIdempotencyConflict
		}
		state, err := m.State(replay.JobID)
		state.IdempotentReplay = true
		return state, err
	}
	if active, _ := m.activeState(); active != nil {
		return State{}, ErrUpdateActive
	}

	jobID := uuid.NewString()
	jobDir := m.jobDir(jobID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return State{}, err
	}
	packagePath := filepath.Join(jobDir, "package.zip")
	if err := os.Rename(tempPath, packagePath); err != nil {
		return State{}, err
	}
	validated, err := ValidateAndExtract(packagePath, filepath.Join(jobDir, "staging"), m.CurrentVersion)
	if err != nil {
		_ = os.RemoveAll(jobDir)
		return State{}, err
	}
	if err := m.ensureUpgradeSpace(filepath.Join(jobDir, "staging")); err != nil {
		_ = os.RemoveAll(jobDir)
		return State{}, err
	}
	now := time.Now().UTC()
	state := State{JobID: jobID, Status: "queued", Phase: "validating", CurrentVersion: m.CurrentVersion,
		TargetVersion: validated.Manifest.Version, PackageSHA256: validated.SHA256, UploadBytes: written, CreatedAt: now, UpdatedAt: now}
	if err := writeState(m.statePath(jobID), state); err != nil {
		return State{}, err
	}
	if err := m.claimActive(jobID); err != nil {
		_ = os.RemoveAll(jobDir)
		return State{}, err
	}
	if err := m.writeIdempotency(idempotencyKey, idempotencyRecord{JobID: jobID, PackageSHA256: packageHash}); err != nil {
		m.releaseActive(jobID)
		_ = os.RemoveAll(jobDir)
		return State{}, err
	}
	worker := filepath.Join(jobDir, "upgrade-worker.exe")
	if err := copyRegularFile(m.ExecutablePath, worker, 0o700); err != nil {
		m.releaseActive(jobID)
		m.failBeforeWorker(jobID, err)
		return State{}, err
	}
	if err := m.launchWorker(worker, jobID); err != nil {
		m.releaseActive(jobID)
		m.failBeforeWorker(jobID, err)
		return State{}, err
	}
	return state, nil
}

func (m *Manager) launchWorker(worker, jobID string) error {
	if m.startWorker != nil {
		return m.startWorker(worker, m.Root, jobID)
	}
	return startUpgradeWorker(worker, m.Root, jobID)
}

func (m *Manager) failBeforeWorker(jobID string, cause error) {
	now := time.Now().UTC()
	_ = m.updateState(jobID, "failed", "validating", func(state *State) {
		state.Error = m.sanitizedError(cause)
		state.Rollback = "not_required"
		state.CompletedAt = &now
	})
}

func (m *Manager) ensureUpgradeSpace(staging string) error {
	var required uint64 = 128 << 20
	for _, root := range []string{staging, m.Root} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if root == m.Root && (samePath(path, filepath.Join(m.Root, "data")) || samePath(path, filepath.Join(m.Root, ".git"))) {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				required += uint64(info.Size())
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("measure upgrade files: %w", err)
		}
	}
	for _, filename := range []string{m.DatabasePath, m.DatabasePath + "-wal", m.DatabasePath + "-shm", m.ConfigPath} {
		if info, err := os.Stat(filename); err == nil && info.Mode().IsRegular() {
			required += uint64(info.Size())
		}
	}
	usage, err := disk.Usage(m.Root)
	if err != nil {
		return fmt.Errorf("query free disk space: %w", err)
	}
	if usage.Free < required {
		return fmt.Errorf("insufficient disk space for update snapshot: need %d bytes, have %d", required, usage.Free)
	}
	return nil
}

func (m *Manager) CreateFromFile(zipPath string) (State, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	return m.Create(f, "cli:"+uuid.NewString())
}

func (m *Manager) updateState(jobID, status, phase string, mutate func(*State)) error {
	state, err := m.State(jobID)
	if err != nil {
		return err
	}
	if status != "" {
		state.Status = status
	}
	if phase != "" {
		state.Phase = phase
	}
	state.UpdatedAt = time.Now().UTC()
	if mutate != nil {
		mutate(&state)
	}
	return writeState(m.statePath(jobID), state)
}

func (m *Manager) jobDir(jobID string) string {
	return filepath.Join(m.Root, "data", "update", "jobs", jobID)
}
func (m *Manager) statePath(jobID string) string { return filepath.Join(m.jobDir(jobID), "state.json") }
func (m *Manager) incomingDir() string           { return filepath.Join(m.Root, "data", "update", "incoming") }
func (m *Manager) activePath() string            { return filepath.Join(m.Root, "data", "update", "active.json") }

func (m *Manager) listStates() ([]State, error) {
	entries, err := os.ReadDir(filepath.Join(m.Root, "data", "update", "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := m.State(entry.Name())
		if err == nil {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].CreatedAt.After(states[j].CreatedAt) })
	return states, nil
}

func (m *Manager) activeState() (*State, error) {
	var active struct {
		JobID string `json:"job_id"`
	}
	if err := readJSON(m.activePath(), &active); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	state, err := m.State(active.JobID)
	if err != nil || (state.Status != "queued" && state.Status != "running") {
		_ = os.Remove(m.activePath())
		return nil, nil
	}
	if time.Since(state.UpdatedAt) > 15*time.Minute {
		now := time.Now().UTC()
		_ = m.updateState(state.JobID, "failed", state.Phase, func(value *State) {
			value.Error = "portable updater stopped before reporting a terminal result"
			value.CompletedAt = &now
		})
		_ = os.Remove(m.activePath())
		return nil, nil
	}
	return &state, nil
}

func (m *Manager) claimActive(jobID string) error {
	if err := os.MkdirAll(filepath.Dir(m.activePath()), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(m.activePath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return ErrUpdateActive
	}
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(map[string]string{"job_id": jobID})
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (m *Manager) releaseActive(jobID string) {
	active, err := m.activeState()
	if err == nil && active != nil && active.JobID == jobID {
		_ = os.Remove(m.activePath())
	}
}

type idempotencyRecord struct {
	JobID         string `json:"job_id"`
	PackageSHA256 string `json:"package_sha256"`
}

func (m *Manager) idempotencyPath(key string) string {
	digest := sha256.Sum256([]byte(key))
	return filepath.Join(m.Root, "data", "update", "idempotency", hex.EncodeToString(digest[:])+".json")
}

func (m *Manager) lookupIdempotency(key string) (idempotencyRecord, bool, error) {
	var record idempotencyRecord
	err := readJSON(m.idempotencyPath(key), &record)
	if errors.Is(err, os.ErrNotExist) {
		return record, false, nil
	}
	return record, err == nil, err
}

func (m *Manager) writeIdempotency(key string, record idempotencyRecord) error {
	filename := m.idempotencyPath(key)
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return err
	}
	return writeJSONAtomic(filename, record)
}

func readState(filename string) (State, error) {
	var state State
	err := readJSON(filename, &state)
	return state, err
}

func writeState(filename string, state State) error { return writeJSONAtomic(filename, state) }

func readJSON(filename string, target any) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSONAtomic(filename string, value any) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(filename), ".state-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if err := json.NewEncoder(temp).Encode(value); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceFile(tempName, filename)
}

func copyRegularFile(source, target string, mode os.FileMode) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temp := target + ".new"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(temp)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return replaceFile(temp, target)
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs))
}

func runCommand(executable string, args ...string) error {
	command := exec.Command(executable, args...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}
