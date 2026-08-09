package containerupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
)

const (
	defaultDockerSocket = "/var/run/docker.sock"
	defaultUpdateRoot   = "/app/updates"
	defaultDatabasePath = "/app/data/l4d2panel.sqlite3"
)

var (
	releaseVersionPattern         = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	sensitiveDiagnosticAssignment = regexp.MustCompile(`(?i)(password|secret|token|authorization|cookie|master[_-]?key|setup[_-]?key)(["']?\s*[:=]\s*["']?)([^\s,;}"']+)`)
	bearerDiagnosticValue         = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
)

type Manager struct {
	Socket         string
	Root           string
	DatabasePath   string
	CurrentVersion string
	ContainerID    string
	client         *dockerClient
	initErr        error
	mu             sync.Mutex
}

type workerSpec struct {
	JobID              string `json:"job_id"`
	CurrentContainerID string `json:"current_container_id"`
	CurrentName        string `json:"current_name"`
	TargetImageID      string `json:"target_image_id"`
	TargetImageRef     string `json:"target_image_ref"`
	TargetVersion      string `json:"target_version"`
	DatabasePath       string `json:"database_path"`
}

type idempotencyRecord struct {
	JobID       string `json:"job_id"`
	PackageHash string `json:"package_sha256"`
}

func Configured() bool {
	value, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("CONTAINER_UPDATE_ENABLED")))
	return value
}

func Detect(databasePath, currentVersion string) *Manager {
	socket := strings.TrimSpace(os.Getenv("CONTAINER_DOCKER_SOCKET"))
	if socket == "" {
		socket = defaultDockerSocket
	}
	root := strings.TrimSpace(os.Getenv("CONTAINER_UPDATE_ROOT"))
	if root == "" {
		root = defaultUpdateRoot
	}
	containerID, _ := os.Hostname()
	manager := &Manager{Socket: socket, Root: root, DatabasePath: databasePath, CurrentVersion: currentVersion, ContainerID: containerID}
	manager.client, manager.initErr = newDockerClient(socket)
	return manager
}

func (m *Manager) PackageFormat() string { return "tar.gz" }

func (m *Manager) MaxPackageBytes() int64 { return MaxPackageBytes }

func (m *Manager) Supported() (bool, string) {
	if m.initErr != nil {
		return false, "Docker Socket 不可用或不安全"
	}
	if m.Socket != defaultDockerSocket || m.Root != defaultUpdateRoot || filepath.Clean(m.DatabasePath) != defaultDatabasePath {
		return false, "容器升级只支持固定 Docker Socket、升级卷和数据库路径"
	}
	if !regexp.MustCompile(`^[0-9a-f]{12,64}$`).MatchString(m.ContainerID) {
		return false, "无法识别当前 Control 容器"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.client.ping(ctx); err != nil {
		return false, "Docker Engine API 不可用"
	}
	if _, err := m.client.negotiate(ctx); err != nil {
		return false, "Docker Engine API 版本不受支持"
	}
	current, err := m.client.inspectContainer(ctx, m.ContainerID)
	if err != nil {
		return false, "无法读取当前 Control 容器"
	}
	labels := stringMap(current.Config["Labels"])
	if labels["com.docker.compose.service"] != "app" || labels["com.docker.compose.project"] == "" {
		return false, "当前容器不是受支持的 Compose app 服务"
	}
	socketMount, socketOK := findMount(current.Mounts, defaultDockerSocket)
	if !socketOK || socketMount.Type != "bind" || socketMount.Source != defaultDockerSocket ||
		!hasMount(current.Mounts, "volume", "", "/app/data") || !hasMount(current.Mounts, "volume", "", "/app/spool") ||
		!hasMount(current.Mounts, "volume", "", defaultUpdateRoot) {
		return false, "当前容器缺少 Docker Socket、数据卷、传输卷或升级卷"
	}
	return true, ""
}

func (m *Manager) Overview() portableupdate.Overview {
	overview := portableupdate.Overview{CurrentVersion: m.CurrentVersion, Deployment: "docker-compose", PackageFormat: "tar.gz", MaxPackageSize: MaxPackageBytes, History: []portableupdate.State{}}
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

func (m *Manager) State(jobID string) (portableupdate.State, error) {
	if _, err := uuid.Parse(jobID); err != nil {
		return portableupdate.State{}, os.ErrNotExist
	}
	return readState(m.statePath(jobID))
}

func (m *Manager) PendingReconciliation() []portableupdate.State {
	states, _ := m.listStates()
	result := make([]portableupdate.State, 0)
	for _, state := range states {
		if !state.Reconciled && (state.Status == "succeeded" || state.Status == "failed") {
			result = append(result, state)
		}
	}
	return result
}

func (m *Manager) MarkReconciled(jobID string) error {
	state, err := m.State(jobID)
	if err != nil {
		return err
	}
	state.Reconciled = true
	state.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(m.statePath(jobID), state)
}

func (m *Manager) Create(reader io.Reader, idempotencyKey string) (portableupdate.State, error) {
	if supported, _ := m.Supported(); !supported {
		return portableupdate.State{}, portableupdate.ErrUnsupported
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		return portableupdate.State{}, errors.New("Idempotency-Key is required and must not exceed 200 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if active, activeErr := m.activeState(); active != nil {
		return portableupdate.State{}, portableupdate.ErrUpdateActive
	} else if activeErr != nil && !errors.Is(activeErr, os.ErrNotExist) {
		return portableupdate.State{}, activeErr
	}
	if err := os.MkdirAll(filepath.Join(m.Root, "incoming"), 0o700); err != nil {
		return portableupdate.State{}, err
	}
	temporary, err := os.CreateTemp(filepath.Join(m.Root, "incoming"), "upload-*.tar.gz")
	if err != nil {
		return portableupdate.State{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	_ = temporary.Chmod(0o600)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(reader, MaxPackageBytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return portableupdate.State{}, firstError(copyErr, syncErr, closeErr)
	}
	if written <= 0 || written > MaxPackageBytes {
		return portableupdate.State{}, fmt.Errorf("update package must be no larger than %d bytes", MaxPackageBytes)
	}
	packageHash := hex.EncodeToString(hash.Sum(nil))
	idempotencySum := sha256.Sum256([]byte(idempotencyKey))
	idempotencyPath := filepath.Join(m.Root, "idempotency", hex.EncodeToString(idempotencySum[:])+".json")
	if replay, err := readIdempotency(idempotencyPath); err == nil {
		if replay.PackageHash != packageHash {
			return portableupdate.State{}, portableupdate.ErrIdempotencyConflict
		}
		state, stateErr := m.State(replay.JobID)
		state.IdempotentReplay = true
		return state, stateErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return portableupdate.State{}, err
	}

	validated, err := validatePackage(temporaryPath)
	if err != nil {
		return portableupdate.State{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	engine, err := m.client.negotiate(ctx)
	if err != nil {
		return portableupdate.State{}, err
	}
	if engine.OS != "linux" || normalizeArchitecture(engine.Arch) != validated.Architecture {
		return portableupdate.State{}, fmt.Errorf("update image platform linux/%s does not match Docker host %s/%s", validated.Architecture, engine.OS, engine.Arch)
	}
	if err := rejectDowngrade(m.CurrentVersion, validated.Version); err != nil {
		return portableupdate.State{}, err
	}
	current, err := m.client.inspectContainer(ctx, m.ContainerID)
	if err != nil {
		return portableupdate.State{}, err
	}

	jobID := uuid.NewString()
	jobDir := m.jobDir(jobID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return portableupdate.State{}, err
	}
	packagePath := filepath.Join(jobDir, "package.tar.gz")
	if err := os.Rename(temporaryPath, packagePath); err != nil {
		return portableupdate.State{}, err
	}
	now := time.Now().UTC()
	state := portableupdate.State{JobID: jobID, Status: "queued", Phase: "validating", CurrentVersion: m.CurrentVersion,
		TargetVersion: validated.Version, PackageSHA256: packageHash, UploadBytes: written, CreatedAt: now, UpdatedAt: now}
	if err := writeJSONAtomic(m.statePath(jobID), state); err != nil {
		return portableupdate.State{}, err
	}
	if err := writeJSONAtomic(m.activePath(), map[string]string{"job_id": jobID}); err != nil {
		return portableupdate.State{}, err
	}
	if err := os.MkdirAll(filepath.Dir(idempotencyPath), 0o700); err != nil {
		m.failBeforeWorker(state, err)
		return portableupdate.State{}, err
	}
	if err := writeJSONAtomic(idempotencyPath, idempotencyRecord{JobID: jobID, PackageHash: packageHash}); err != nil {
		m.failBeforeWorker(state, err)
		return portableupdate.State{}, err
	}

	packageFile, err := os.Open(packagePath)
	if err != nil {
		m.failBeforeWorker(state, err)
		return portableupdate.State{}, err
	}
	err = m.client.loadImage(ctx, packageFile)
	_ = packageFile.Close()
	if err != nil {
		m.failAfterImageLoad(state, current.Image, err)
		return portableupdate.State{}, err
	}
	loaded, err := m.client.inspectImage(ctx, validated.ImageRef)
	if err != nil || loaded.OS != validated.OS || normalizeArchitecture(loaded.Architecture) != validated.Architecture || loaded.Config.Labels["org.opencontainers.image.version"] != validated.Version {
		if err == nil {
			err = errors.New("loaded image metadata does not match the validated package")
		}
		m.failAfterImageLoad(state, current.Image, err)
		return portableupdate.State{}, err
	}
	spec := workerSpec{JobID: jobID, CurrentContainerID: current.ID, CurrentName: strings.TrimPrefix(current.Name, "/"),
		TargetImageID: loaded.ID, TargetImageRef: loaded.ID, TargetVersion: validated.Version, DatabasePath: m.DatabasePath}
	if err := writeJSONAtomic(filepath.Join(jobDir, "worker.json"), spec); err != nil {
		m.failAfterImageLoad(state, current.Image, err)
		return portableupdate.State{}, err
	}
	_ = os.Remove(packagePath)
	workerID, err := m.createWorker(ctx, current, jobID)
	if err == nil {
		err = m.client.startContainer(ctx, workerID)
	}
	if err != nil {
		if workerID != "" {
			_ = m.client.removeContainer(context.Background(), workerID, true)
		}
		m.failAfterImageLoad(state, current.Image, err)
		return portableupdate.State{}, err
	}
	return state, nil
}

func (m *Manager) createWorker(ctx context.Context, current containerInspect, jobID string) (string, error) {
	dataMount, ok := findMount(current.Mounts, "/app/data")
	if !ok || dataMount.Type != "volume" || dataMount.Name == "" {
		return "", errors.New("current Control data volume is unavailable")
	}
	updateMount, ok := findMount(current.Mounts, defaultUpdateRoot)
	if !ok || updateMount.Type != "volume" || updateMount.Name == "" {
		return "", errors.New("current Control update volume is unavailable")
	}
	spoolMount, ok := findMount(current.Mounts, "/app/spool")
	if !ok || spoolMount.Type != "volume" || spoolMount.Name == "" {
		return "", errors.New("current Control spool volume is unavailable")
	}
	socketMount, ok := findMount(current.Mounts, defaultDockerSocket)
	if !ok || socketMount.Type != "bind" || socketMount.Source != defaultDockerSocket {
		return "", errors.New("current Control Docker socket mount is unavailable")
	}
	request := map[string]any{
		"Image":      current.Image,
		"Entrypoint": []string{"/usr/local/bin/l4d2panel"},
		"Cmd":        []string{"container-update-worker", "--job-id", jobID},
		"User":       "0:0",
		"WorkingDir": "/app",
		"Env": []string{
			"CONTAINER_UPDATE_ENABLED=true",
			"CONTAINER_DOCKER_SOCKET=" + defaultDockerSocket,
			"CONTAINER_UPDATE_ROOT=" + defaultUpdateRoot,
			"DATABASE_PATH=" + defaultDatabasePath,
			"MASTER_KEY_FILE=/app/data/control-master.key",
		},
		"Labels": map[string]string{"io.l4d2panel.container-update-worker": jobID},
		"HostConfig": map[string]any{
			"AutoRemove":     true,
			"NetworkMode":    "none",
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"CapAdd":         []string{"CHOWN", "FOWNER", "SETGID", "SETUID"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Binds":          []string{defaultDockerSocket + ":" + defaultDockerSocket + ":rw"},
			"Mounts": []map[string]any{
				{"Type": "volume", "Source": dataMount.Name, "Target": "/app/data", "ReadOnly": false},
				{"Type": "volume", "Source": spoolMount.Name, "Target": "/app/spool", "ReadOnly": false},
				{"Type": "volume", "Source": updateMount.Name, "Target": defaultUpdateRoot, "ReadOnly": false},
			},
			"Tmpfs": map[string]string{"/tmp": "size=64m,mode=1777"},
		},
	}
	return m.client.createContainer(ctx, "l4d2panel-update-"+strings.ReplaceAll(jobID, "-", "")[:20], request)
}

func (m *Manager) failBeforeWorker(state portableupdate.State, cause error) {
	now := time.Now().UTC()
	state.Status, state.Phase, state.Rollback, state.Error = "failed", "validating", "not_required", sanitizeError(cause)
	state.UpdatedAt, state.CompletedAt = now, &now
	_ = writeJSONAtomic(m.statePath(state.JobID), state)
	_ = os.Remove(m.activePath())
}

func (m *Manager) failAfterImageLoad(state portableupdate.State, currentImageID string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.client.tagImage(ctx, currentImageID, "l4d2panel/control", "offline"); err != nil {
		cause = errors.Join(cause, fmt.Errorf("restore offline image tag: %w", err))
	}
	m.failBeforeWorker(state, cause)
}

func (m *Manager) activeState() (*portableupdate.State, error) {
	var active map[string]string
	if err := readJSON(m.activePath(), &active); err != nil {
		return nil, err
	}
	state, err := m.State(active["job_id"])
	if err != nil {
		return nil, err
	}
	if state.Status != "queued" && state.Status != "running" {
		_ = os.Remove(m.activePath())
		return nil, os.ErrNotExist
	}
	return &state, nil
}

func (m *Manager) listStates() ([]portableupdate.State, error) {
	entries, err := os.ReadDir(filepath.Join(m.Root, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	states := make([]portableupdate.State, 0, len(entries))
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

func (m *Manager) jobDir(jobID string) string    { return filepath.Join(m.Root, "jobs", jobID) }
func (m *Manager) statePath(jobID string) string { return filepath.Join(m.jobDir(jobID), "state.json") }
func (m *Manager) activePath() string            { return filepath.Join(m.Root, "active.json") }

func readState(path string) (portableupdate.State, error) {
	var state portableupdate.State
	err := readJSON(path, &state)
	return state, err
}

func readIdempotency(path string) (idempotencyRecord, error) {
	var record idempotencyRecord
	err := readJSON(path, &record)
	return record, err
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(target)
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, path)
}

func hasMount(mounts []dockerMount, kind, name, destination string) bool {
	for _, mount := range mounts {
		if mount.Type == kind && mount.Destination == destination && (name == "" || mount.Name == name) {
			return true
		}
	}
	return false
}

func findMount(mounts []dockerMount, destination string) (dockerMount, bool) {
	for _, mount := range mounts {
		if mount.Destination == destination {
			return mount, true
		}
	}
	return dockerMount{}, false
}

func stringMap(value any) map[string]string {
	result := map[string]string{}
	if source, ok := value.(map[string]any); ok {
		for key, raw := range source {
			if text, ok := raw.(string); ok {
				result[key] = text
			}
		}
	}
	return result
}

func normalizeArchitecture(value string) string {
	switch value {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return value
	}
}

func rejectDowngrade(current, target string) error {
	if current == target {
		return nil
	}
	currentVersion, currentOK := parseReleaseVersion(current)
	targetVersion, targetOK := parseReleaseVersion(target)
	if !currentOK {
		// Development versions do not carry an ordered release number. Allow a
		// trusted owner to replace one with another development build or move to
		// a formal release; package identity and platform checks still apply.
		return nil
	}
	if !targetOK {
		return errors.New("release container builds cannot update to an unordered development version")
	}
	for index := range currentVersion {
		if targetVersion[index] < currentVersion[index] {
			return errors.New("container update downgrade is not allowed")
		}
		if targetVersion[index] > currentVersion[index] {
			return nil
		}
	}
	return nil
}

func parseReleaseVersion(value string) ([3]uint64, bool) {
	match := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 4 {
		return [3]uint64{}, false
	}
	var result [3]uint64
	for index := range result {
		number, err := strconv.ParseUint(match[index+1], 10, 64)
		if err != nil {
			return [3]uint64{}, false
		}
		result[index] = number
	}
	return result, true
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	message := sanitizeDiagnosticText(err.Error())
	runes := []rune(message)
	if len(runes) > 500 {
		message = string(runes[:500])
	}
	return message
}

func sanitizeDiagnosticText(value string) string {
	value = sensitiveDiagnosticAssignment.ReplaceAllString(value, `$1$2[redacted]`)
	value = bearerDiagnosticValue.ReplaceAllString(value, "Bearer [redacted]")
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' || character >= 0x20 {
			return character
		}
		return -1
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
