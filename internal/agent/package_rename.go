package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) renamePackage(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	get := func(key string) string { value, _ := job.Payload[key].(string); return value }
	name, source, target, kind, sha := get("name"), get("path"), get("target_path"), get("kind"), get("expected_sha256")
	if err := domain.ValidatePackageName(name); err != nil {
		return nil, err
	}
	if get("package_id") == "" {
		return nil, errors.New("package ID is required")
	}
	result := map[string]any{"name": name, "path": target, "sha256": sha}
	if source == target {
		return result, nil
	}
	if e.processes == nil || packageProcessActive(e.processes.State(job.InstanceID).Status) {
		return nil, errors.New("instance must be stopped before renaming a package file")
	}
	approved, oldCanonical, newCanonical, err := domain.PackageRenamePaths(kind, source, path.Base(target))
	if err != nil {
		return nil, err
	}
	if approved != target {
		return nil, ErrPathEscape
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	root, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	actual, _, err := hashRegularFileWithinNoFollow(root, source, maxPackageDownload)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(actual, sha) {
		return nil, errors.New("package content changed since it was recorded")
	}
	// Reject either state of the new basename, including unmanaged files. This
	// prevents a rename from making the next enable/disable collide with a file.
	otherTarget := newCanonical
	if target == newCanonical {
		if kind == "vpk" || kind == "workshop" {
			otherTarget = path.Dir(target) + "/panel-disabled/" + path.Base(target)
		} else {
			otherTarget = path.Dir(target) + "/disabled/" + path.Base(target)
		}
	}
	if err := requirePackageRenameDestination(root, otherTarget); err != nil {
		return nil, err
	}
	if err := requirePackageRenameDestination(root, target); err != nil {
		return nil, err
	}
	type move struct{ source, target string }
	moves := []move{}
	snapshots, _ := job.Payload["snapshots"].([]any)
	if len(snapshots) > packageVersionRetention {
		return nil, errors.New("too many package snapshots")
	}
	for _, raw := range snapshots {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid package snapshot")
		}
		storage, _ := item["storage_path"].(string)
		expected, _ := item["sha256"].(string)
		renamed, err := domain.RenamedPackageSnapshot(job.InstanceID, oldCanonical, newCanonical, storage)
		if err != nil {
			return nil, err
		}
		actual, _, err := hashRegularFileWithinNoFollow(e.config.DataDir, storage, maxPackageDownload)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(actual, expected) {
			return nil, errors.New("package snapshot checksum mismatch")
		}
		if err := requirePackageRenameDestination(e.config.DataDir, renamed); err != nil {
			return nil, err
		}
		moves = append(moves, move{storage, renamed})
	}
	completed := []move{}
	rollback := func(cause error) (map[string]any, error) {
		for i := len(completed) - 1; i >= 0; i-- {
			if err := moveFileWithinNoFollow(e.config.DataDir, completed[i].target, completed[i].source); err != nil {
				cause = errors.Join(cause, fmt.Errorf("restore package snapshot: %w", err))
			}
		}
		return nil, cause
	}
	for _, m := range moves {
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		if err := moveFileWithinNoFollow(e.config.DataDir, m.source, m.target); err != nil {
			return rollback(err)
		}
		completed = append(completed, m)
	}
	if err := ctx.Err(); err != nil {
		return rollback(err)
	}
	if err := moveFileWithinNoFollow(root, source, target); err != nil {
		return rollback(err)
	}
	progress(100, "package renamed")
	return result, nil
}

func requirePackageRenameDestination(root, relative string) error {
	file, err := openManagedFileNoFollow(root, relative)
	if err == nil {
		file.Close()
		return errors.New("目标文件名已存在，不能覆盖")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
