package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) deletePackage(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(payload.Kind, "smart") {
		return e.deleteSmartPlugin(job, progress)
	}
	if strings.EqualFold(payload.Kind, "sourcemod") || strings.EqualFold(payload.Kind, "metamod") {
		return e.deleteServerExtensionPackage(ctx, job, payload, progress)
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	relative, err := approvedDeletedPackageTarget(payload.Kind, firstNonEmpty(payload.PackagePath, payload.Path))
	if err != nil {
		return nil, err
	}
	expectedSHA := strings.ToLower(strings.TrimSpace(payload.ExpectedSHA256))
	if len(expectedSHA) != 64 {
		return nil, errors.New("package deletion requires the recorded SHA-256")
	}
	if _, err := hex.DecodeString(expectedSHA); err != nil {
		return nil, errors.New("package deletion requires the recorded SHA-256")
	}
	actualSHA, _, err := hashRegularFileWithinNoFollow(instancePath, relative, maxPackageDownload)
	if err != nil {
		return nil, fmt.Errorf("read package before deletion: %w", err)
	}
	if !strings.EqualFold(actualSHA, expectedSHA) {
		return nil, errors.New("package content changed since it was recorded; refresh or reinstall it before deleting")
	}
	for _, storagePath := range payload.SnapshotPaths {
		if _, err := e.resolvePackageSnapshotForDelete(job.InstanceID, storagePath); err != nil {
			return nil, err
		}
	}
	if err := deleteManagedFileNoFollow(instancePath, relative); err != nil {
		return nil, fmt.Errorf("delete package file: %w", err)
	}
	result := map[string]any{
		"path": relative, "sha256": actualSHA,
		"restart_required": e.packageServerRunning(job.InstanceID),
	}
	removedSnapshots := make([]string, 0, len(payload.SnapshotPaths))
	for _, storagePath := range payload.SnapshotPaths {
		if err := deleteManagedFileNoFollow(e.config.DataDir, filepath.ToSlash(storagePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result["retention_warning"] = "one or more saved package versions could not be removed"
			continue
		}
		removedSnapshots = append(removedSnapshots, filepath.ToSlash(storagePath))
	}
	if len(removedSnapshots) > 0 {
		result["removed_snapshots"] = removedSnapshots
	}
	progress(100, "package deleted")
	return result, nil
}

func (e *Executor) deleteServerExtensionPackage(ctx context.Context, job domain.Job, payload operationPayload, progress ProgressFunc) (map[string]any, error) {
	if e.smartPluginServerRunning(job.InstanceID) {
		return nil, errors.New("instance must be stopped before deleting a server extension package")
	}
	if len(payload.ManagedFiles) == 0 || len(payload.ManagedFiles) > maxPackageFiles {
		return nil, errors.New("server extension deletion requires a bounded managed file inventory")
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	expectedSHA := strings.ToLower(strings.TrimSpace(payload.ExpectedSHA256))
	if len(expectedSHA) != 64 {
		return nil, errors.New("server extension deletion requires the recorded package SHA-256")
	}
	if _, err := hex.DecodeString(expectedSHA); err != nil {
		return nil, errors.New("server extension deletion requires the recorded package SHA-256")
	}
	seen := make(map[string]bool, len(payload.ManagedFiles))
	for _, file := range payload.ManagedFiles {
		key := strings.ToLower(filepath.ToSlash(file.Path))
		if seen[key] || validateServerExtensionManagedTarget(payload.Kind, file.Path) != nil || len(file.SHA256) != 64 || file.Size < 0 {
			return nil, errors.New("server extension deletion contains an invalid managed file")
		}
		seen[key] = true
		if _, decodeErr := hex.DecodeString(file.SHA256); decodeErr != nil {
			return nil, errors.New("server extension deletion contains an invalid file SHA-256")
		}
		sha, size, hashErr := hashRegularFileWithinNoFollow(instancePath, file.Path, maxPackageExpanded)
		if hashErr != nil || !strings.EqualFold(sha, file.SHA256) || size != file.Size {
			return nil, fmt.Errorf("server extension file %q changed or is missing; deletion was refused", file.Path)
		}
	}
	progress(20, "creating component deletion rollback checkpoint")
	checkpointJob := backupJobWithKind(job, "checkpoint")
	checkpointJob.ID += "-component-delete-checkpoint"
	checkpoint, err := e.backup(ctx, checkpointJob, func(int, string) {})
	if err != nil {
		return nil, fmt.Errorf("create component deletion checkpoint: %w", err)
	}
	checkpointPath := stringFromMap(checkpoint, "path")
	if checkpointPath == "" {
		return nil, errors.New("component deletion checkpoint is missing its archive path")
	}
	progress(55, "deleting managed server extension files")
	for _, file := range payload.ManagedFiles {
		if err := deleteManagedFileNoFollow(instancePath, file.Path); err != nil {
			stagingDir, stagingErr := ensureDataSubdirectory(e.config.DataDir, "staging")
			if stagingErr == nil {
				stagingErr = restoreCheckpointExact(instancePath, checkpointPath, filepath.Join(stagingDir, "restore-component-delete-"+safeLogName(job.ID)), job.InstanceID)
			}
			if stagingErr != nil {
				return nil, fmt.Errorf("delete server extension file %q: %w; rollback also failed: %v", file.Path, err, stagingErr)
			}
			return nil, fmt.Errorf("delete server extension file %q and restored checkpoint: %w", file.Path, err)
		}
	}
	progress(100, "server extension package deleted")
	result := map[string]any{"package_id": stringFromPayload(job.Payload, "package_id"), "path": "", "sha256": expectedSHA, "files": len(payload.ManagedFiles)}
	if strings.EqualFold(payload.Kind, "sourcemod") {
		result["preserved_files"] = []string{sourceModAdminsSimplePath}
	}
	return result, nil
}

func validateServerExtensionManagedTarget(kind, value string) error {
	if err := validateServerExtensionInstallTarget(kind, value); err != nil {
		return err
	}
	if strings.EqualFold(value, sourceModAdminsSimplePath) {
		return errors.New("the protected SourceMod administrators file is not a managed deletion target")
	}
	return nil
}

func validateServerExtensionInstallTarget(kind, value string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(value))))
	if cleaned != value || filepath.IsAbs(filepath.FromSlash(cleaned)) || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, `\`) {
		return errors.New("invalid server extension path")
	}
	lower := strings.ToLower(cleaned)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "metamod":
		if lower == "left4dead2/addons/metamod.vdf" || lower == "left4dead2/addons/metamod_x64.vdf" || strings.HasPrefix(lower, "left4dead2/addons/metamod/") {
			return nil
		}
	case "sourcemod":
		if lower == "left4dead2/addons/metamod/sourcemod.vdf" || strings.HasPrefix(lower, "left4dead2/addons/sourcemod/") || strings.HasPrefix(lower, "left4dead2/cfg/sourcemod/") {
			return nil
		}
	}
	return errors.New("server extension path is outside its fixed component directory")
}

func approvedDeletedPackageTarget(kind, raw string) (string, error) {
	relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(raw))))
	if relative == "." || filepath.IsAbs(filepath.FromSlash(relative)) || strings.HasPrefix(relative, "../") {
		return "", errors.New("package path is invalid")
	}
	base := filepath.Base(filepath.FromSlash(relative))
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "vpk", "workshop":
		if !strings.HasSuffix(strings.ToLower(base), ".vpk") || isReservedMapAddonBase(base) {
			return "", errors.New("only ordinary VPK package files can be deleted")
		}
		enabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", base))
		disabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", "panel-disabled", base))
		if relative != enabled && relative != disabled {
			return "", errors.New("VPK package path is outside the approved package directories")
		}
	case "sourcemod", "smx", "plugin":
		if !strings.HasSuffix(strings.ToLower(base), ".smx") {
			return "", errors.New("only SourceMod SMX package files can be deleted")
		}
		enabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", base))
		disabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", "disabled", base))
		if relative != enabled && relative != disabled {
			return "", errors.New("SourceMod package path is outside the approved package directories")
		}
	default:
		return "", errors.New("package deletion supports VPK and SourceMod SMX packages")
	}
	return relative, nil
}
