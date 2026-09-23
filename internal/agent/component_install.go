package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const metamodDependencyError = "SourceMod requires an installed Metamod:Source; install and verify Metamod:Source first"

const sourceModAdminsSimplePath = "left4dead2/addons/sourcemod/configs/admins_simple.ini"

func requireMetamodInstalled(instancePath string) error {
	vdf, err := readRegularFileWithinNoFollow(instancePath, "left4dead2/addons/metamod.vdf", 64<<10)
	if err != nil || !strings.Contains(strings.ToLower(string(vdf)), "metamod/bin/server") {
		return errors.New(metamodDependencyError)
	}
	binary := "left4dead2/addons/metamod/bin/server.so"
	if runtime.GOOS == "windows" {
		binary = "left4dead2/addons/metamod/bin/server.dll"
	}
	file, err := openManagedFileNoFollow(instancePath, binary)
	if err != nil {
		return errors.New(metamodDependencyError)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New(metamodDependencyError)
	}
	return nil
}

func (e *Executor) installServerExtension(ctx context.Context, job domain.Job, payload operationPayload, record InstanceRecord, instancePath, componentKind, localSource, downloadedHash string, downloadedSize int64, version, stagedRoot string, stagedFiles []string, progress ProgressFunc) (map[string]any, error) {
	if payload.RCONPassword == "" {
		return nil, errors.New("RCON password is required to verify a server extension installation")
	}
	if len(payload.ManagedFiles) > maxPackageFiles {
		return nil, errors.New("existing server extension inventory contains too many files")
	}
	packageID := stringFromPayload(job.Payload, "package_id")
	if packageID == "" {
		payload.ManagedFiles = nil
	} else {
		seen := make(map[string]bool, len(payload.ManagedFiles))
		for _, file := range payload.ManagedFiles {
			key := strings.ToLower(filepath.ToSlash(file.Path))
			if seen[key] || validateServerExtensionManagedTarget(componentKind, file.Path) != nil || len(file.SHA256) != 64 || file.Size < 0 {
				return nil, errors.New("existing server extension inventory contains an invalid managed file")
			}
			if _, err := hex.DecodeString(file.SHA256); err != nil {
				return nil, errors.New("existing server extension inventory contains an invalid file SHA-256")
			}
			seen[key] = true
		}
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

	progress(45, "installing extension package from validated staging")
	stagingDir, installErr := ensureDataSubdirectory(e.config.DataDir, "staging")
	var files, preserved []string
	if installErr == nil && stagedRoot != "" {
		files, preserved, installErr = installStagedPackage(instancePath, stagedRoot, stagedFiles, packageArchiveInstallOptions{
			AllowNative: true, PackageID: packageID, RegisteredPaths: payload.RegisteredPaths,
			ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget(componentKind, value) },
			PreserveExisting: func() map[string]bool {
				if componentKind == "sourcemod" {
					return map[string]bool{sourceModAdminsSimplePath: true}
				}
				return nil
			}(),
		})
	} else if installErr == nil {
		options := packageArchiveInstallOptions{
			AllowNative: true, PackageID: packageID, RegisteredPaths: payload.RegisteredPaths,
			ValidateTarget: func(value string) error { return validateServerExtensionInstallTarget(componentKind, value) },
		}
		if componentKind == "sourcemod" {
			options.PreserveExisting = map[string]bool{sourceModAdminsSimplePath: true}
		}
		files, preserved, installErr = installPackageArchive(instancePath, localSource, stagingDir, options)
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

	progress(95, "inventorying installed server extension")
	managedFiles := make([]map[string]any, 0, len(files)+len(payload.ManagedFiles))
	managedPaths := make(map[string]bool, len(files))
	for _, file := range files {
		if strings.EqualFold(file, sourceModAdminsSimplePath) {
			continue
		}
		managedPaths[strings.ToLower(filepath.ToSlash(file))] = true
		sha, size, hashErr := hashRegularFileWithinNoFollow(instancePath, file, maxPackageExpanded)
		if hashErr != nil {
			rollbackErr := e.rollbackServerExtension(job, checkpointPath, wasRunning)
			if rollbackErr != nil {
				return nil, fmt.Errorf("inventory installed extension file %q: %w; rollback/recovery also failed: %v", file, hashErr, rollbackErr)
			}
			return nil, fmt.Errorf("inventory installed extension file %q and rolled back: %w", file, hashErr)
		}
		managedFiles = append(managedFiles, map[string]any{"path": file, "sha256": sha, "size": size})
	}
	for _, previous := range payload.ManagedFiles {
		key := strings.ToLower(filepath.ToSlash(previous.Path))
		if managedPaths[key] || validateServerExtensionManagedTarget(componentKind, previous.Path) != nil {
			continue
		}
		sha, size, hashErr := hashRegularFileWithinNoFollow(instancePath, previous.Path, maxPackageExpanded)
		if hashErr != nil {
			if errors.Is(hashErr, os.ErrNotExist) {
				preserved = append(preserved, previous.Path)
				continue
			}
			rollbackErr := e.rollbackServerExtension(job, checkpointPath, wasRunning)
			if rollbackErr != nil {
				return nil, fmt.Errorf("inventory existing extension file %q: %w; rollback/recovery also failed: %v", previous.Path, hashErr, rollbackErr)
			}
			return nil, fmt.Errorf("inventory existing extension file %q and rolled back: %w", previous.Path, hashErr)
		}
		if !strings.EqualFold(sha, previous.SHA256) || size != previous.Size {
			preserved = append(preserved, previous.Path)
			continue
		}
		managedPaths[key] = true
		managedFiles = append(managedFiles, map[string]any{"path": previous.Path, "sha256": sha, "size": size})
	}
	sort.Slice(managedFiles, func(i, j int) bool {
		return strings.ToLower(stringFromMap(managedFiles[i], "path")) < strings.ToLower(stringFromMap(managedFiles[j], "path"))
	})
	if len(managedFiles) == 0 {
		rollbackErr := e.rollbackServerExtension(job, checkpointPath, wasRunning)
		if rollbackErr != nil {
			return nil, fmt.Errorf("server extension produced no managed files; rollback/recovery also failed: %v", rollbackErr)
		}
		return nil, errors.New("server extension produced no managed files and was rolled back")
	}
	progress(100, "server extension installed and verified")
	result := map[string]any{
		"kind": componentKind, "files": files, "sha256": downloadedHash, "size": downloadedSize,
		"checkpoint": checkpoint, "verified": true, "verification_command": extensionVerificationCommand(componentKind),
		"running": wasRunning, "managed_files": managedFiles, "file_count": len(managedFiles), "package_id": packageID,
	}
	if len(preserved) > 0 {
		result["preserved_files"] = preserved
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
	if port := gameEndpointPort(record, payload); port <= 0 || port > 65535 {
		return errors.New("invalid RCON port for extension verification")
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		response, err := executeLocalRCONOnA2SHost(attemptCtx, record, payload, payload.RCONPassword, command, 2*time.Second, 4*time.Second)
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
			return fmt.Errorf("%s did not verify %s within 30 seconds: %w", command, kind, lastErr)
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
	} else if manifest, manifestErr := ReadBackupManifest(checkpointPath); manifestErr != nil {
		restored = false
		failures = append(failures, fmt.Errorf("read checkpoint map references: %w", manifestErr))
	} else if len(manifest.Maps) != 0 && e.maps == nil {
		restored = false
		failures = append(failures, errors.New("restore checkpoint map assignments: map library is unavailable"))
	} else if e.maps != nil {
		_, mapErr := e.maps.restoreBackupAssignments(job.InstanceID, record, instancePath, manifest.Maps)
		if mapErr == nil {
			// Continue with running-state recovery below.
		} else {
			restored = false
			failures = append(failures, fmt.Errorf("restore checkpoint map assignments: %w", mapErr))
		}
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
