package containerupdate

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
)

type backupManifest struct {
	Files []string `json:"files"`
}

func RunWorker(logger *slog.Logger, args []string) (resultErr error) {
	flags := flag.NewFlagSet("container-update-worker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jobID := flags.String("job-id", "", "container update job ID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("container-update-worker requires --job-id")
	}
	if _, err := uuid.Parse(*jobID); err != nil {
		return errors.New("container-update-worker received an invalid job ID")
	}
	root := strings.TrimSpace(os.Getenv("CONTAINER_UPDATE_ROOT"))
	socket := strings.TrimSpace(os.Getenv("CONTAINER_DOCKER_SOCKET"))
	if root != defaultUpdateRoot || socket != defaultDockerSocket {
		return errors.New("container update worker requires fixed update and Docker Socket paths")
	}
	manager := &Manager{Root: root, Socket: socket, DatabasePath: defaultDatabasePath}
	client, err := newDockerClient(socket)
	if err != nil {
		return err
	}
	manager.client = client
	defer func() {
		if resultErr == nil {
			return
		}
		state, stateErr := manager.State(*jobID)
		if stateErr == nil && (state.Status == "queued" || state.Status == "running") {
			_ = finishWorkerFailure(manager, state, state.Phase, "not_required", resultErr)
		}
	}()
	var spec workerSpec
	if err := readJSON(filepath.Join(manager.jobDir(*jobID), "worker.json"), &spec); err != nil {
		return err
	}
	if spec.JobID != *jobID || spec.DatabasePath != defaultDatabasePath || spec.CurrentContainerID == "" || spec.CurrentName == "" || spec.TargetImageID == "" {
		return errors.New("container update worker specification is invalid")
	}
	state, err := manager.State(*jobID)
	if err != nil {
		return err
	}
	if state.Status != "queued" {
		return errors.New("container update job is not queued")
	}

	// Let the HTTP upload response reach the browser before stopping Control.
	time.Sleep(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if _, err := client.negotiate(ctx); err != nil {
		return finishWorkerFailure(manager, state, "validating", "not_required", err)
	}
	current, err := client.inspectContainer(ctx, spec.CurrentContainerID)
	if err != nil {
		return finishWorkerFailure(manager, state, "validating", "not_required", err)
	}
	labels := stringMap(current.Config["Labels"])
	if strings.TrimPrefix(current.Name, "/") != spec.CurrentName || labels["com.docker.compose.service"] != "app" || labels["com.docker.compose.project"] == "" {
		return finishWorkerFailure(manager, state, "validating", "not_required", errors.New("current container identity changed before update"))
	}
	target, err := client.inspectImage(ctx, spec.TargetImageID)
	if err != nil || target.ID != spec.TargetImageID || target.OS != "linux" || target.Config.Labels["org.opencontainers.image.title"] != "L4D2 Panel Control" || target.Config.Labels["org.opencontainers.image.version"] != spec.TargetVersion {
		if err == nil {
			err = errors.New("target image identity changed before update")
		}
		return finishWorkerFailure(manager, state, "validating", "not_required", err)
	}
	replacement, err := replacementRequest(current, spec.TargetImageRef)
	if err != nil {
		return finishWorkerFailure(manager, state, "validating", "not_required", err)
	}

	state.Status, state.Phase, state.UpdatedAt = "running", "stopping", time.Now().UTC()
	if err := writeJSONAtomic(manager.statePath(*jobID), state); err != nil {
		return err
	}
	if current.State.Running {
		if err := client.stopContainer(ctx, current.ID, 45); err != nil {
			return finishWorkerFailure(manager, state, "stopping", "not_required", err)
		}
	}

	state.Phase, state.UpdatedAt = "backing_up", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(*jobID), state)
	if err := snapshotDatabase(manager.jobDir(*jobID), spec.DatabasePath); err != nil {
		_ = client.startContainer(context.Background(), current.ID)
		return finishWorkerFailure(manager, state, "backing_up", "not_required", err)
	}

	rollbackName := spec.CurrentName + "-rollback-" + strings.ReplaceAll(*jobID, "-", "")[:12]
	if err := client.renameContainer(ctx, current.ID, rollbackName); err != nil {
		_ = client.startContainer(context.Background(), current.ID)
		return finishWorkerFailure(manager, state, "installing", "not_required", err)
	}
	state.Phase, state.UpdatedAt = "installing", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(*jobID), state)
	newID, installErr := client.createContainer(ctx, spec.CurrentName, replacement)
	if installErr == nil {
		installErr = client.startContainer(ctx, newID)
	}
	if installErr != nil {
		return rollbackContainerUpdate(ctx, manager, state, current.ID, current.Image, spec.CurrentName, newID, installErr)
	}

	state.Phase, state.UpdatedAt = "migrating", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(*jobID), state)
	time.Sleep(2 * time.Second)
	state.Phase, state.UpdatedAt = "health_check", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(*jobID), state)
	if err := waitContainerReady(ctx, client, newID, spec.TargetImageID, 2*time.Minute); err != nil {
		return rollbackContainerUpdate(ctx, manager, state, current.ID, current.Image, spec.CurrentName, newID, err)
	}
	now := time.Now().UTC()
	state.Status, state.Phase, state.Rollback, state.Error = "succeeded", "health_check", "not_required", ""
	state.UpdatedAt, state.CompletedAt = now, &now
	if err := writeJSONAtomic(manager.statePath(*jobID), state); err != nil {
		return rollbackContainerUpdate(ctx, manager, state, current.ID, current.Image, spec.CurrentName, newID, err)
	}
	if err := client.removeContainer(ctx, current.ID, false); err != nil {
		logger.Warn("updated Control is healthy but old stopped container could not be removed", "container_id", current.ID, "error", err)
	}
	_ = os.Remove(manager.activePath())
	return nil
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func replacementRequest(current containerInspect, targetImage string) (map[string]any, error) {
	labels := stringMap(current.Config["Labels"])
	project := strings.TrimSpace(labels["com.docker.compose.project"])
	if labels["com.docker.compose.service"] != "app" || project == "" || len(project) > 63 {
		return nil, errors.New("current Compose identity is invalid")
	}
	if len(current.NetworkSettings.Networks) != 1 {
		return nil, errors.New("current Control must use exactly one Compose network")
	}
	var networkName string
	for name := range current.NetworkSettings.Networks {
		networkName = name
	}
	if networkName == "" || len(networkName) > 127 || strings.ContainsAny(networkName, "\x00\r\n") {
		return nil, errors.New("current Compose network is invalid")
	}

	dataMount, dataOK := findMount(current.Mounts, "/app/data")
	spoolMount, spoolOK := findMount(current.Mounts, "/app/spool")
	updateMount, updateOK := findMount(current.Mounts, defaultUpdateRoot)
	socketMount, socketOK := findMount(current.Mounts, defaultDockerSocket)
	if !validNamedVolume(dataMount, dataOK) || !validNamedVolume(spoolMount, spoolOK) || !validNamedVolume(updateMount, updateOK) ||
		!socketOK || socketMount.Type != "bind" || socketMount.Source != defaultDockerSocket {
		return nil, errors.New("current Control mounts do not match the fixed Compose layout")
	}

	environment, err := replacementEnvironment(current.Config["Env"])
	if err != nil {
		return nil, err
	}
	portBindings, err := replacementPortBindings(current.HostConfig["PortBindings"])
	if err != nil {
		return nil, err
	}
	composeLabels := map[string]string{}
	for key, value := range labels {
		if strings.HasPrefix(key, "com.docker.compose.") && key != "com.docker.compose.image" {
			composeLabels[key] = value
		}
	}

	hostConfig := map[string]any{
		"AutoRemove":     false,
		"RestartPolicy":  map[string]any{"Name": "unless-stopped", "MaximumRetryCount": 0},
		"NetworkMode":    networkName,
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
		"Tmpfs":        map[string]string{"/tmp": "size=64m,mode=1777"},
		"PortBindings": portBindings,
		"LogConfig":    map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "20m", "max-file": "5"}},
	}
	return map[string]any{
		"Image":            targetImage,
		"User":             "0:0",
		"Env":              environment,
		"Labels":           composeLabels,
		"ExposedPorts":     map[string]any{"19090/tcp": map[string]any{}},
		"Healthcheck":      map[string]any{"Test": []string{"CMD", "wget", "-qO-", "http://127.0.0.1:19090/healthz"}, "Interval": int64(10 * time.Second), "Timeout": int64(3 * time.Second), "Retries": 12},
		"StopSignal":       "SIGTERM",
		"StopTimeout":      45,
		"HostConfig":       hostConfig,
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{networkName: map[string]any{"Aliases": []string{"app"}}}},
	}, nil
}

func validNamedVolume(mount dockerMount, exists bool) bool {
	return exists && mount.Type == "volume" && mount.Name != "" && len(mount.Name) <= 255 && !strings.ContainsAny(mount.Name, "\x00\r\n/\\")
}

func replacementEnvironment(raw any) ([]string, error) {
	var source []string
	if err := remarshal(raw, &source); err != nil {
		return nil, errors.New("current Control environment is invalid")
	}
	allowed := map[string]bool{
		"DATABASE_PATH": true, "MASTER_KEY": true, "MASTER_KEY_FILE": true, "CONTAINER_UPDATE_ENABLED": true,
		"CONTAINER_DOCKER_SOCKET": true, "CONTAINER_UPDATE_ROOT": true, "PUBLIC_URL": true, "COOKIE_SECURE": true,
		"TRUSTED_PROXIES": true, "BOOTSTRAP_USERNAME": true, "BOOTSTRAP_PASSWORD": true, "BOOTSTRAP_DISPLAY_NAME": true,
		"SESSION_TTL": true, "LOG_LEVEL": true, "TZ": true,
	}
	values := map[string]string{}
	for _, entry := range source {
		key, value, found := strings.Cut(entry, "=")
		if !found || !allowed[key] || len(entry) > 8192 || strings.ContainsAny(entry, "\x00\r\n") {
			continue
		}
		values[key] = value
	}
	required := map[string]string{
		"DATABASE_PATH": defaultDatabasePath, "MASTER_KEY_FILE": "/app/data/control-master.key", "CONTAINER_UPDATE_ENABLED": "true",
		"CONTAINER_DOCKER_SOCKET": defaultDockerSocket, "CONTAINER_UPDATE_ROOT": defaultUpdateRoot,
	}
	for key, expected := range required {
		if values[key] != expected {
			return nil, fmt.Errorf("current Control environment %s is invalid", key)
		}
	}
	result := make([]string, 0, len(values))
	for _, key := range []string{"DATABASE_PATH", "MASTER_KEY", "MASTER_KEY_FILE", "CONTAINER_UPDATE_ENABLED", "CONTAINER_DOCKER_SOCKET", "CONTAINER_UPDATE_ROOT", "PUBLIC_URL", "COOKIE_SECURE", "TRUSTED_PROXIES", "BOOTSTRAP_USERNAME", "BOOTSTRAP_PASSWORD", "BOOTSTRAP_DISPLAY_NAME", "SESSION_TTL", "LOG_LEVEL", "TZ"} {
		if value, exists := values[key]; exists {
			result = append(result, key+"="+value)
		}
	}
	return result, nil
}

func replacementPortBindings(raw any) (map[string][]dockerPortBinding, error) {
	var bindings map[string][]dockerPortBinding
	if err := remarshal(raw, &bindings); err != nil || len(bindings) != 1 || len(bindings["19090/tcp"]) != 1 {
		return nil, errors.New("current Control port binding is invalid")
	}
	binding := bindings["19090/tcp"][0]
	port, err := strconv.ParseUint(binding.HostPort, 10, 16)
	if err != nil || port == 0 || (binding.HostIP != "" && net.ParseIP(binding.HostIP) == nil) {
		return nil, errors.New("current Control host port binding is invalid")
	}
	return map[string][]dockerPortBinding{"19090/tcp": {binding}}, nil
}

func remarshal(source any, target any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func rollbackContainerUpdate(ctx context.Context, manager *Manager, state portableupdate.State, oldID, oldImageID, originalName, newID string, cause error) error {
	state.Phase, state.UpdatedAt = "rolling_back", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(state.JobID), state)
	rollbackErr := error(nil)
	if newID != "" {
		cause = replacementFailureDiagnostic(ctx, manager.client, newID, cause)
		_ = manager.client.stopContainer(ctx, newID, 10)
		if err := manager.client.removeContainer(ctx, newID, true); err != nil {
			rollbackErr = err
		}
	}
	if err := restoreDatabase(manager.jobDir(state.JobID), defaultDatabasePath); err != nil && rollbackErr == nil {
		rollbackErr = err
	}
	if err := manager.client.renameContainer(ctx, oldID, originalName); err != nil && rollbackErr == nil {
		rollbackErr = err
	}
	if rollbackErr == nil {
		if err := manager.client.startContainer(ctx, oldID); err != nil {
			rollbackErr = err
		} else if err := waitContainerReady(ctx, manager.client, oldID, "", 90*time.Second); err != nil {
			rollbackErr = err
		}
	}
	if err := manager.client.tagImage(ctx, oldImageID, "l4d2panel/control", "offline"); err != nil && rollbackErr == nil {
		rollbackErr = err
	}
	rollback := "succeeded"
	if rollbackErr != nil {
		rollback = "failed"
		cause = fmt.Errorf("%v; rollback failed: %w", cause, rollbackErr)
	}
	return finishWorkerFailure(manager, state, "rolling_back", rollback, cause)
}

func replacementFailureDiagnostic(ctx context.Context, client *dockerClient, containerID string, cause error) error {
	parts := []string{cause.Error()}
	if inspected, err := client.inspectContainer(ctx, containerID); err == nil {
		detail := fmt.Sprintf("replacement exit_code=%d", inspected.State.ExitCode)
		if inspected.State.OOMKilled {
			detail += " oom_killed=true"
		}
		if engineError := sanitizeDiagnosticText(inspected.State.Error); engineError != "" {
			detail += " engine_error=" + engineError
		}
		parts = append(parts, detail)
	}
	if logs, err := client.containerLogs(ctx, containerID, 32<<10); err == nil {
		if excerpt := lastDiagnosticRunes(sanitizeDiagnosticText(dockerLogText(logs)), 320); excerpt != "" {
			parts = append(parts, "replacement_log="+excerpt)
		}
	}
	return errors.New(strings.Join(parts, "; "))
}

func dockerLogText(data []byte) string {
	if len(data) < 8 || (data[0] != 1 && data[0] != 2) || data[1] != 0 || data[2] != 0 || data[3] != 0 {
		return string(data)
	}
	var output strings.Builder
	for len(data) >= 8 {
		if (data[0] != 1 && data[0] != 2) || data[1] != 0 || data[2] != 0 || data[3] != 0 {
			break
		}
		size := int(binary.BigEndian.Uint32(data[4:8]))
		data = data[8:]
		if size < 0 || size > len(data) {
			break
		}
		output.Write(data[:size])
		data = data[size:]
	}
	return output.String()
}

func lastDiagnosticRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return "…" + string(runes[len(runes)-limit:])
}

func finishWorkerFailure(manager *Manager, state portableupdate.State, phase, rollback string, cause error) error {
	now := time.Now().UTC()
	state.Status, state.Phase, state.Rollback, state.Error = "failed", phase, rollback, sanitizeError(cause)
	state.UpdatedAt, state.CompletedAt = now, &now
	_ = writeJSONAtomic(manager.statePath(state.JobID), state)
	_ = os.Remove(manager.activePath())
	return cause
}

func waitContainerReady(ctx context.Context, client *dockerClient, id, expectedImageID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		container, err := client.inspectContainer(ctx, id)
		if err == nil {
			if expectedImageID != "" && container.Image != expectedImageID {
				return errors.New("replacement container is not running the validated image")
			}
			if !container.State.Running {
				return errors.New("replacement container exited before becoming ready")
			}
			if err := client.execReady(ctx, id); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("replacement container did not become ready before timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func snapshotDatabase(jobDir, databasePath string) error {
	backupDir := filepath.Join(jobDir, "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	manifest := backupManifest{}
	for _, source := range []string{databasePath, databasePath + "-wal", databasePath + "-shm", "/app/data/control-master.key"} {
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("backup source is not a regular file: %s", filepath.Base(source))
		}
		name := filepath.Base(source)
		if err := copyFile(source, filepath.Join(backupDir, name), 0o600); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, name)
	}
	if len(manifest.Files) == 0 {
		return errors.New("database backup did not find any persistent files")
	}
	return writeJSONAtomic(filepath.Join(backupDir, "manifest.json"), manifest)
}

func restoreDatabase(jobDir, databasePath string) error {
	backupDir := filepath.Join(jobDir, "backup")
	var manifest backupManifest
	if err := readJSON(filepath.Join(backupDir, "manifest.json"), &manifest); err != nil {
		return err
	}
	allowed := map[string]string{
		filepath.Base(databasePath): databasePath, filepath.Base(databasePath + "-wal"): databasePath + "-wal",
		filepath.Base(databasePath + "-shm"): databasePath + "-shm", "control-master.key": "/app/data/control-master.key",
	}
	for _, target := range allowed {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range manifest.Files {
		target, exists := allowed[name]
		if !exists {
			return errors.New("database backup manifest contains an unexpected file")
		}
		if err := copyFile(filepath.Join(backupDir, name), target, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("update backup source must be a regular file")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(destination)
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		_ = os.Remove(destination)
		return err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func cloneMap(source map[string]any) map[string]any {
	data, _ := json.Marshal(source)
	result := map[string]any{}
	_ = json.Unmarshal(data, &result)
	return result
}
