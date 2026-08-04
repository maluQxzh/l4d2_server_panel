package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const packageVersionRetention = 3

type packageFileSnapshot struct {
	StoragePath string
	TargetPath  string
	SHA256      string
	Size        int64
	CreatedAt   time.Time
}

func (snapshot packageFileSnapshot) result() map[string]any {
	return map[string]any{
		"storage_path": snapshot.StoragePath,
		"target_path":  snapshot.TargetPath,
		"sha256":       snapshot.SHA256,
		"size":         snapshot.Size,
		"created_at":   snapshot.CreatedAt.UTC().Format(time.RFC3339Nano),
		"manifest": map[string]any{"files": []any{map[string]any{
			"path": snapshot.TargetPath, "sha256": snapshot.SHA256, "size": snapshot.Size,
		}}},
	}
}

// installVersionedPackage performs the single-file VPK/SMX mutation while the
// server is stopped. An existing file is copied into the Agent-owned version
// store first. Any copy, validation, restart, or SourceMod verification failure
// restores that file before returning an error.
func (e *Executor) installVersionedPackage(ctx context.Context, job domain.Job, payload operationPayload, kind, instancePath, source, targetRelative, expectedSHA string, progress ProgressFunc) (*packageFileSnapshot, []string, error) {
	targetRelative, err := approvedVersionedPackageTarget(kind, targetRelative)
	if err != nil {
		return nil, nil, err
	}
	target, err := ResolveWithin(instancePath, filepath.FromSlash(targetRelative))
	if err != nil {
		return nil, nil, err
	}
	if err := validateRegularPackageSource(source); err != nil {
		return nil, nil, err
	}
	if err := validateVersionedPackageFile(kind, source, expectedSHA); err != nil {
		return nil, nil, err
	}
	if isVPKPackageKind(kind) {
		if err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{{
			Source: source, TargetRelative: targetRelative,
		}}); err != nil {
			return nil, nil, err
		}
	}

	wasRunning := e.packageServerRunning(job.InstanceID)
	if wasRunning {
		progress(62, "stopping server before package replacement")
		if _, err := e.stop(ctx, job, func(int, string) {}); err != nil {
			return nil, nil, fmt.Errorf("stop server before package replacement: %w", err)
		}
	}

	var previous *packageFileSnapshot
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() || isReparseOrSymlink(info) {
			e.recoverPackageRunningState(job, wasRunning)
			return nil, nil, errors.New("existing package target is not a regular file")
		}
		progress(68, "preserving the installed package version")
		snapshot, snapshotErr := e.createPackageSnapshot(job.InstanceID, job.ID, targetRelative, target)
		if snapshotErr != nil {
			recoveryErr := e.recoverPackageRunningState(job, wasRunning)
			if recoveryErr != nil {
				return nil, nil, fmt.Errorf("preserve installed package: %w; restart recovery failed: %v", snapshotErr, recoveryErr)
			}
			return nil, nil, fmt.Errorf("preserve installed package: %w", snapshotErr)
		}
		previous = &snapshot
	} else if !os.IsNotExist(statErr) {
		e.recoverPackageRunningState(job, wasRunning)
		return nil, nil, statErr
	}

	progress(76, "replacing package atomically")
	mutationErr := copyRegularFileToRootNoFollow(source, instancePath, targetRelative, secureWriteOptions{
		Mode: 0o640, ExpectedSHA256: expectedSHA, Replace: true, RejectExistingLink: true,
	})
	if mutationErr == nil {
		sha, _, hashErr := hashRegularFileWithinNoFollow(instancePath, targetRelative, maxPackageDownload)
		if hashErr != nil || !strings.EqualFold(sha, expectedSHA) {
			mutationErr = errors.New("installed package checksum changed after commit")
		}
	}
	if mutationErr == nil && e.packageMutationVerifier != nil {
		mutationErr = e.packageMutationVerifier(kind, target)
	}
	if mutationErr == nil && wasRunning {
		progress(88, "restarting server after package replacement")
		_, mutationErr = e.start(ctx, job, func(int, string) {})
		if mutationErr == nil && isSourceModPluginKind(kind) {
			mutationErr = e.verifySourceModAvailable(ctx, job, payload)
		}
	}
	if mutationErr != nil {
		rollbackErr := e.rollbackPackageMutation(job, instancePath, targetRelative, previous, wasRunning)
		if previous != nil {
			_ = e.removePackageSnapshot(*previous)
		}
		if rollbackErr != nil {
			return nil, nil, fmt.Errorf("package replacement failed: %w; rollback/recovery also failed: %v", mutationErr, rollbackErr)
		}
		return nil, nil, fmt.Errorf("package replacement failed and was rolled back: %w", mutationErr)
	}

	pruned, pruneErr := e.prunePackageSnapshots(job.InstanceID, targetRelative)
	if pruneErr != nil {
		rollbackErr := e.rollbackPackageMutation(job, instancePath, targetRelative, previous, wasRunning)
		if previous != nil {
			_ = e.removePackageSnapshot(*previous)
		}
		if rollbackErr != nil {
			return nil, nil, fmt.Errorf("enforce package version retention: %w; rollback/recovery also failed: %v", pruneErr, rollbackErr)
		}
		return nil, nil, fmt.Errorf("package replacement was rolled back because version retention failed: %w", pruneErr)
	}
	return previous, pruned, nil
}

func (e *Executor) rollbackPackage(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	targetRelative, err := approvedVersionedPackageTarget(payload.Kind, firstNonEmpty(payload.PackagePath, payload.Path))
	if err != nil {
		return nil, err
	}
	if len(payload.ExpectedSHA256) != 64 {
		return nil, errors.New("rollback version SHA-256 is invalid")
	}
	target, err := ResolveWithin(instancePath, filepath.FromSlash(targetRelative))
	if err != nil {
		return nil, err
	}
	snapshotPath, err := e.resolvePackageSnapshot(job.InstanceID, targetRelative, payload.SnapshotPath)
	if err != nil {
		return nil, err
	}
	if err := validateRegularPackageSource(snapshotPath); err != nil {
		return nil, fmt.Errorf("saved package version is unavailable: %w", err)
	}
	if err := validateVersionedPackageFile(payload.Kind, snapshotPath, payload.ExpectedSHA256); err != nil {
		return nil, fmt.Errorf("saved package version failed verification: %w", err)
	}
	if isVPKPackageKind(payload.Kind) {
		if err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{{
			Source: snapshotPath, TargetRelative: targetRelative,
		}}); err != nil {
			return nil, fmt.Errorf("saved VPK version conflicts with enabled addons: %w", err)
		}
	}
	currentSHA, _, err := hashRegularFileWithinNoFollow(instancePath, targetRelative, maxPackageDownload)
	if err != nil {
		return nil, fmt.Errorf("read current package: %w", err)
	}
	if strings.EqualFold(currentSHA, payload.ExpectedSHA256) {
		return nil, errors.New("the selected package version is already installed")
	}

	wasRunning := e.packageServerRunning(job.InstanceID)
	if wasRunning {
		progress(15, "stopping server before package rollback")
		if _, err := e.stop(ctx, job, func(int, string) {}); err != nil {
			return nil, fmt.Errorf("stop server before package rollback: %w", err)
		}
	}
	progress(35, "preserving the current package version")
	previous, err := e.createPackageSnapshot(job.InstanceID, job.ID, targetRelative, target)
	if err != nil {
		recoveryErr := e.recoverPackageRunningState(job, wasRunning)
		if recoveryErr != nil {
			return nil, fmt.Errorf("preserve current package: %w; restart recovery failed: %v", err, recoveryErr)
		}
		return nil, fmt.Errorf("preserve current package: %w", err)
	}

	progress(55, "restoring the selected package version")
	mutationErr := copyRegularFileToRootNoFollow(snapshotPath, instancePath, targetRelative, secureWriteOptions{
		Mode: 0o640, ExpectedSHA256: payload.ExpectedSHA256, Replace: true, RejectExistingLink: true,
	})
	if mutationErr == nil {
		sha, _, hashErr := hashRegularFileWithinNoFollow(instancePath, targetRelative, maxPackageDownload)
		if hashErr != nil || !strings.EqualFold(sha, payload.ExpectedSHA256) {
			mutationErr = errors.New("restored package checksum changed after commit")
		}
	}
	if mutationErr == nil && e.packageMutationVerifier != nil {
		mutationErr = e.packageMutationVerifier(payload.Kind, target)
	}
	if mutationErr == nil && wasRunning {
		progress(75, "restarting server after package rollback")
		_, mutationErr = e.start(ctx, job, func(int, string) {})
		if mutationErr == nil && isSourceModPluginKind(payload.Kind) {
			mutationErr = e.verifySourceModAvailable(ctx, job, payload)
		}
	}
	if mutationErr != nil {
		rollbackErr := e.rollbackPackageMutation(job, instancePath, targetRelative, &previous, wasRunning)
		_ = e.removePackageSnapshot(previous)
		if rollbackErr != nil {
			return nil, fmt.Errorf("package rollback failed: %w; recovery also failed: %v", mutationErr, rollbackErr)
		}
		return nil, fmt.Errorf("package rollback failed and the current version was restored: %w", mutationErr)
	}

	result := map[string]any{
		"kind":              strings.ToLower(strings.TrimSpace(payload.Kind)),
		"path":              targetRelative,
		"sha256":            strings.ToLower(payload.ExpectedSHA256),
		"version":           payload.RestoreVersion,
		"previous_version":  previous.result(),
		"consumed_snapshot": filepath.ToSlash(payload.SnapshotPath),
		"restart_verified":  wasRunning,
	}
	if removeErr := deleteManagedFileNoFollow(e.config.DataDir, filepath.ToSlash(payload.SnapshotPath)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		result["retention_warning"] = "restored snapshot could not be removed"
	}
	pruned, pruneErr := e.prunePackageSnapshots(job.InstanceID, targetRelative)
	if pruneErr == nil && len(pruned) > 0 {
		result["pruned_snapshots"] = pruned
	}
	progress(100, "package version restored")
	return result, nil
}

func (e *Executor) createPackageSnapshot(instanceID, jobID, targetRelative, source string) (packageFileSnapshot, error) {
	root, err := ensureDataSubdirectory(e.config.DataDir, "package-versions")
	if err != nil {
		return packageFileSnapshot{}, err
	}
	instanceKey := packageVersionKey(instanceID)
	targetKey := packageVersionKey(targetRelative)
	fileName := packageVersionKey(jobID) + ".bin"
	storageRelative := filepath.ToSlash(filepath.Join(instanceKey, targetKey, fileName))
	if err := copyRegularFileToRootNoFollow(source, root, storageRelative, secureWriteOptions{Mode: 0o600, Replace: false}); err != nil {
		return packageFileSnapshot{}, err
	}
	destination := filepath.Join(root, filepath.FromSlash(storageRelative))
	sha, size, err := hashRegularFileWithinNoFollow(root, storageRelative, maxPackageDownload)
	if err != nil {
		_ = deleteManagedFileNoFollow(root, storageRelative)
		return packageFileSnapshot{}, err
	}
	relative, err := filepath.Rel(e.config.DataDir, destination)
	if err != nil || strings.HasPrefix(relative, "..") {
		_ = deleteManagedFileNoFollow(root, storageRelative)
		return packageFileSnapshot{}, ErrPathEscape
	}
	return packageFileSnapshot{StoragePath: filepath.ToSlash(relative), TargetPath: targetRelative, SHA256: sha, Size: size, CreatedAt: time.Now().UTC()}, nil
}

func (e *Executor) resolvePackageSnapshot(instanceID, targetRelative, storageRelative string) (string, error) {
	storageRelative = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(storageRelative))))
	expectedPrefix := filepath.ToSlash(filepath.Join("package-versions", packageVersionKey(instanceID), packageVersionKey(targetRelative))) + "/"
	if !strings.HasPrefix(storageRelative, expectedPrefix) {
		return "", errors.New("saved package version is outside its Agent-owned version directory")
	}
	base := strings.TrimPrefix(storageRelative, expectedPrefix)
	if strings.Contains(base, "/") || len(base) != 68 || !strings.HasSuffix(base, ".bin") {
		return "", errors.New("saved package version name is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimSuffix(base, ".bin")); err != nil {
		return "", errors.New("saved package version name is invalid")
	}
	resolved, err := resolveBelow(e.config.DataDir, filepath.FromSlash(storageRelative))
	if err != nil {
		return "", errors.New("saved package version escapes the Agent data directory")
	}
	return resolved, nil
}

func (e *Executor) prunePackageSnapshots(instanceID, targetRelative string) ([]string, error) {
	root, err := ensureDataSubdirectory(e.config.DataDir, "package-versions")
	if err != nil {
		return nil, err
	}
	directory, err := ResolveWithin(root, filepath.Join(packageVersionKey(instanceID), packageVersionKey(targetRelative)))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type candidate struct {
		name string
		when time.Time
	}
	versions := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) != 68 || !strings.HasSuffix(entry.Name(), ".bin") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) {
			continue
		}
		versions = append(versions, candidate{name: entry.Name(), when: info.ModTime()})
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].when.Equal(versions[j].when) {
			return versions[i].name > versions[j].name
		}
		return versions[i].when.After(versions[j].when)
	})
	if len(versions) <= packageVersionRetention {
		return nil, nil
	}
	removed := make([]string, 0)
	for _, version := range versions[packageVersionRetention:] {
		storageRelative := filepath.ToSlash(filepath.Join(packageVersionKey(instanceID), packageVersionKey(targetRelative), version.name))
		if removeErr := deleteManagedFileNoFollow(root, storageRelative); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removed, removeErr
		}
		removed = append(removed, filepath.ToSlash(filepath.Join("package-versions", storageRelative)))
	}
	return removed, nil
}

func (e *Executor) removePackageSnapshot(snapshot packageFileSnapshot) error {
	// This helper only receives metadata returned by createPackageSnapshot; the
	// deletion itself is anchored below data_dir and never follows links.
	if err := deleteManagedFileNoFollow(e.config.DataDir, snapshot.StoragePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (e *Executor) rollbackPackageMutation(job domain.Job, instancePath, targetRelative string, previous *packageFileSnapshot, wasRunning bool) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if e.packageServerRunning(job.InstanceID) {
		if _, err := e.stop(cleanupCtx, job, func(int, string) {}); err != nil {
			return fmt.Errorf("stop changed server: %w", err)
		}
	}
	if previous == nil {
		if err := deleteManagedFileNoFollow(instancePath, targetRelative); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		source, err := resolveBelow(e.config.DataDir, filepath.FromSlash(previous.StoragePath))
		if err != nil {
			return err
		}
		if err := copyRegularFileToRootNoFollow(source, instancePath, targetRelative, secureWriteOptions{
			Mode: 0o640, ExpectedSHA256: previous.SHA256, Replace: true, RejectExistingLink: true,
		}); err != nil {
			return err
		}
		sha, _, err := hashRegularFileWithinNoFollow(instancePath, targetRelative, maxPackageDownload)
		if err != nil || !strings.EqualFold(sha, previous.SHA256) {
			return errors.New("restored package checksum does not match the preserved version")
		}
	}
	return e.recoverPackageRunningState(job, wasRunning)
}

func (e *Executor) packageServerRunning(instanceID string) bool {
	return e.processes != nil && packageProcessActive(e.processes.State(instanceID).Status)
}

func (e *Executor) recoverPackageRunningState(job domain.Job, wasRunning bool) error {
	if !wasRunning || e.processes == nil {
		return nil
	}
	return e.bestEffortStart(job)
}

func (e *Executor) verifySourceModAvailable(ctx context.Context, job domain.Job, payload operationPayload) error {
	if payload.RCONPassword == "" {
		return errors.New("RCON password is required to verify a SourceMod plugin rollback")
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return errors.New("instance disappeared before SourceMod verification")
	}
	if port := gameEndpointPort(record, payload); port <= 0 || port > 65535 {
		return errors.New("invalid RCON port for SourceMod verification")
	}
	response, err := e.executeInstanceRCON(ctx, record, payload, payload.RCONPassword, "sm plugins list", 8*time.Second)
	if err != nil {
		return err
	}
	lower := strings.ToLower(response)
	if strings.Contains(lower, "unknown command") || strings.Contains(lower, "not found") {
		return errors.New("SourceMod did not respond after the package change")
	}
	return nil
}

func approvedVersionedPackageTarget(kind, target string) (string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	target = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(target))))
	if target == "." || strings.HasPrefix(target, "../") || strings.HasPrefix(target, "/") || strings.Contains(target, `\`) {
		return "", ErrPathEscape
	}
	base := filepath.Base(filepath.FromSlash(target))
	switch {
	case kind == "vpk" || kind == "vpk_url" || kind == "workshop":
		expected := filepath.ToSlash(filepath.Join("left4dead2", "addons", base))
		if target != expected || !strings.HasSuffix(strings.ToLower(base), ".vpk") {
			return "", errors.New("VPK version target is outside the approved addons directory")
		}
	case isSourceModPluginKind(kind):
		expected := filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", base))
		if target != expected || !strings.HasSuffix(strings.ToLower(base), ".smx") {
			return "", errors.New("SourceMod version target is outside the approved plugins directory")
		}
	default:
		return "", errors.New("version rollback supports only VPK and SourceMod SMX packages")
	}
	return target, nil
}

func isSourceModPluginKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return kind == "smx" || kind == "plugin"
}

func validateRegularPackageSource(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || isReparseOrSymlink(info) {
		return errors.New("package source is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maxPackageDownload {
		return errors.New("package source must contain data and cannot exceed 512 MiB")
	}
	return nil
}

func validateVersionedPackageFile(kind, path, expectedSHA string) error {
	if isVPKPackageKind(kind) {
		_, sha, err := validateVPK(path)
		if err != nil {
			return err
		}
		if expectedSHA != "" && !strings.EqualFold(sha, expectedSHA) {
			return errors.New("VPK checksum does not match the expected package")
		}
		return nil
	}
	if !isSourceModPluginKind(kind) {
		return errors.New("versioned plugin must be a SourceMod .smx file")
	}
	sha, _, err := hashFile(path)
	if err != nil {
		return err
	}
	if expectedSHA != "" && !strings.EqualFold(sha, expectedSHA) {
		return errors.New("SourceMod plugin checksum does not match the expected package")
	}
	return nil
}

func packageVersionKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
