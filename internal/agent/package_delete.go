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

func (e *Executor) deletePackage(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(payload.Kind, "smart") {
		return e.deleteSmartPlugin(job, progress)
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
