package containerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	request := replacementRequest(current, spec.TargetImageRef)
	state.Phase, state.UpdatedAt = "installing", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(*jobID), state)
	newID, installErr := client.createContainer(ctx, spec.CurrentName, request)
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

func replacementRequest(current containerInspect, targetImage string) map[string]any {
	config := cloneMap(current.Config)
	config["Image"] = targetImage
	delete(config, "Hostname")
	delete(config, "Domainname")
	hostConfig := cloneMap(current.HostConfig)
	endpoints := make(map[string]map[string]any, len(current.NetworkSettings.Networks))
	for name, source := range current.NetworkSettings.Networks {
		endpoint := map[string]any{}
		for _, key := range []string{"Aliases", "Links", "DriverOpts", "GwPriority"} {
			if value, exists := source[key]; exists && value != nil {
				endpoint[key] = value
			}
		}
		if ipam, exists := source["IPAMConfig"]; exists && ipam != nil {
			endpoint["IPAMConfig"] = ipam
		}
		endpoints[name] = endpoint
	}
	return map[string]any{"Image": targetImage, "HostConfig": hostConfig, "NetworkingConfig": map[string]any{"EndpointsConfig": endpoints},
		"Hostname": config["Hostname"], "Domainname": config["Domainname"], "User": config["User"], "AttachStdin": config["AttachStdin"],
		"AttachStdout": config["AttachStdout"], "AttachStderr": config["AttachStderr"], "ExposedPorts": config["ExposedPorts"],
		"Tty": config["Tty"], "OpenStdin": config["OpenStdin"], "StdinOnce": config["StdinOnce"], "Env": config["Env"], "Cmd": config["Cmd"],
		"Healthcheck": config["Healthcheck"], "ArgsEscaped": config["ArgsEscaped"], "Volumes": config["Volumes"], "WorkingDir": config["WorkingDir"],
		"Entrypoint": config["Entrypoint"], "OnBuild": config["OnBuild"], "Labels": config["Labels"], "StopSignal": config["StopSignal"],
		"StopTimeout": config["StopTimeout"], "Shell": config["Shell"]}
}

func rollbackContainerUpdate(ctx context.Context, manager *Manager, state portableupdate.State, oldID, oldImageID, originalName, newID string, cause error) error {
	state.Phase, state.UpdatedAt = "rolling_back", time.Now().UTC()
	_ = writeJSONAtomic(manager.statePath(state.JobID), state)
	rollbackErr := error(nil)
	if newID != "" {
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
