package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) installServerExtension(ctx context.Context, job domain.Job, payload operationPayload, record InstanceRecord, instancePath, componentKind, localSource, downloadedHash string, downloadedSize int64, version string, progress ProgressFunc) (map[string]any, error) {
	if payload.RCONPassword == "" {
		return nil, errors.New("RCON password is required to verify a server extension installation")
	}
	wasRunning := packageProcessActive(e.processes.State(job.InstanceID).Status)
	if wasRunning {
		progress(15, "stopping server before extension installation")
		if _, err := e.stop(ctx, job, func(p int, message string) { progress(5+p/10, message) }); err != nil {
			return nil, fmt.Errorf("stop server before extension installation: %w", err)
		}
	}

	progress(25, "creating package rollback checkpoint")
	checkpointJob := backupJobWithKind(job, "checkpoint")
	checkpointJob.ID += "-package-checkpoint"
	checkpoint, err := e.backup(ctx, checkpointJob, func(int, string) {})
	if err != nil {
		if wasRunning {
			if recoveryErr := e.bestEffortStart(job); recoveryErr != nil {
				return nil, fmt.Errorf("create extension checkpoint: %w; additionally failed to restore running state: %v", err, recoveryErr)
			}
		}
		return nil, fmt.Errorf("create extension checkpoint: %w", err)
	}
	checkpointPath, ok := checkpoint["path"].(string)
	if !ok || checkpointPath == "" {
		if wasRunning {
			if recoveryErr := e.bestEffortStart(job); recoveryErr != nil {
				return nil, fmt.Errorf("extension checkpoint did not return a recoverable path; additionally failed to restore running state: %v", recoveryErr)
			}
		}
		return nil, errors.New("extension checkpoint did not return a recoverable path")
	}

	progress(45, "extracting extension package safely")
	stagingDir, installErr := ensureDataSubdirectory(e.config.DataDir, "staging")
	var files []string
	if installErr == nil {
		files, installErr = InstallPackageArchive(instancePath, localSource, stagingDir, true)
	}
	if installErr == nil {
		progress(65, "starting server for extension verification")
		_, installErr = e.start(ctx, job, func(p int, message string) { progress(60+p/5, message) })
	}
	if installErr == nil {
		progress(85, "verifying extension through RCON")
		installErr = e.verifyServerExtension(ctx, record, payload, componentKind)
	}
	if installErr == nil && !wasRunning {
		progress(92, "restoring the original stopped state")
		_, installErr = e.stop(ctx, job, func(int, string) {})
	}
	if installErr != nil {
		rollbackErr := e.rollbackServerExtension(job, checkpointPath, wasRunning)
		if rollbackErr != nil {
			return nil, fmt.Errorf("extension installation failed: %w; rollback/recovery also failed: %v", installErr, rollbackErr)
		}
		return nil, fmt.Errorf("extension installation failed and was rolled back: %w", installErr)
	}

	progress(100, "server extension installed and verified")
	result := map[string]any{
		"kind": componentKind, "files": files, "sha256": downloadedHash, "size": downloadedSize,
		"checkpoint": checkpoint, "verified": true, "verification_command": extensionVerificationCommand(componentKind),
		"running": wasRunning,
	}
	if version != "" {
		result["version"] = version
	}
	return result, nil
}

func (e *Executor) verifyServerExtension(ctx context.Context, record InstanceRecord, payload operationPayload, kind string) error {
	command := extensionVerificationCommand(kind)
	if command == "" {
		return fmt.Errorf("unsupported server extension %q", kind)
	}
	host, port := rconEndpoint(record, payload)
	if port <= 0 || port > 65535 {
		return errors.New("invalid RCON port for extension verification")
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		response, err := (RCONClient{Timeout: 4 * time.Second}).Execute(attemptCtx, net.JoinHostPort(host, strconv.Itoa(port)), payload.RCONPassword, command)
		cancel()
		if err == nil {
			lower := strings.ToLower(response)
			expected := kind
			if kind == "metamod" {
				expected = "metamod"
			}
			if strings.Contains(lower, expected) && !strings.Contains(lower, "unknown command") && !strings.Contains(lower, "not found") {
				return nil
			}
			err = fmt.Errorf("%s did not report the expected %s version", command, kind)
		}
		lastErr = err
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func extensionVerificationCommand(kind string) string {
	switch kind {
	case "metamod":
		return "meta version"
	case "sourcemod":
		return "sm version"
	default:
		return ""
	}
}

func packageProcessActive(status string) bool {
	return processStatusActive(status) || status == "crash_loop"
}

func (e *Executor) rollbackServerExtension(job domain.Job, checkpointPath string, wasRunning bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var failures []error
	if packageProcessActive(e.processes.State(job.InstanceID).Status) {
		if _, err := e.stop(cleanupCtx, job, func(int, string) {}); err != nil {
			return fmt.Errorf("stop failed installation before rollback: %w", err)
		}
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return errors.New("instance disappeared before extension rollback")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return err
	}
	stagingDir, err := ensureDataSubdirectory(e.config.DataDir, "staging")
	if err != nil {
		return err
	}
	staging, err := ResolveWithin(stagingDir, "restore-package-"+safeLogName(job.ID))
	if err != nil {
		return err
	}
	restored := true
	if err := restoreCheckpointExact(instancePath, checkpointPath, staging, job.InstanceID); err != nil {
		restored = false
		failures = append(failures, fmt.Errorf("restore checkpoint: %w", err))
	}
	if wasRunning && restored {
		if _, err := e.start(cleanupCtx, job, func(int, string) {}); err != nil {
			failures = append(failures, fmt.Errorf("restore running state: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (e *Executor) bestEffortStart(job domain.Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := e.start(ctx, job, func(int, string) {})
	return err
}

// restoreCheckpointExact first validates and overlays the checkpoint, then
// removes files created after the checkpoint. RestoreBackup is deliberately run
// before any deletion so a corrupt archive cannot destroy the live tree.
func restoreCheckpointExact(instancePath, archivePath, stagingDir, instanceID string) error {
	inspected, err := ReadBackupManifest(archivePath)
	if err != nil {
		return err
	}
	if inspected.InstanceID != instanceID {
		return errors.New("checkpoint belongs to a different instance")
	}
	_, skipped, err := RestoreBackup(instancePath, archivePath, stagingDir)
	if err != nil {
		return err
	}
	if len(skipped) != 0 {
		return errors.New("checkpoint unexpectedly skipped platform-native files")
	}
	return nil
}
