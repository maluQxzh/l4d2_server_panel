package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	managedVPKDirectory = "left4dead2/addons"
	maxScannedVPKs      = 1000
)

func (e *Executor) scanInstalledPackages(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	entries, err := ListManagedFiles(instancePath, managedVPKDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			progress(100, "addons directory does not exist")
			return installedPackageScanResult(nil, nil), nil
		}
		return nil, fmt.Errorf("list managed addons directory: %w", err)
	}
	candidates := make([]ManagedFile, 0)
	for _, entry := range entries {
		name := strings.ToLower(entry.Name)
		if entry.IsDir || !strings.HasSuffix(name, ".vpk") || strings.HasPrefix(name, "l4d2panel-map-") {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) > maxScannedVPKs {
		return nil, fmt.Errorf("addons directory contains more than %d directly installed VPK files", maxScannedVPKs)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return strings.ToLower(candidates[i].Name) < strings.ToLower(candidates[j].Name)
	})

	packages := make([]any, 0, len(candidates))
	rejected := make([]any, 0)
	for index, entry := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		progress(5+(index*90)/max(1, len(candidates)), "validating installed VPKs")
		file, openErr := openManagedFileNoFollow(instancePath, entry.Path)
		if openErr != nil {
			rejected = append(rejected, scannedPackageRejection(entry.Name, openErr))
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			if statErr == nil {
				statErr = errors.New("VPK is not a regular file")
			}
			rejected = append(rejected, scannedPackageRejection(entry.Name, statErr))
			continue
		}
		version, sha, _, validateErr := validateVPKFile(file)
		closeErr := file.Close()
		if validateErr == nil {
			validateErr = closeErr
		}
		if validateErr != nil {
			rejected = append(rejected, scannedPackageRejection(entry.Name, validateErr))
			continue
		}
		packages = append(packages, map[string]any{
			"name": entry.Name, "path": entry.Path, "sha256": sha,
			"size": info.Size(), "vpk_version": version,
		})
	}
	progress(100, "installed VPK scan completed")
	return installedPackageScanResult(packages, rejected), nil
}

func installedPackageScanResult(packages, rejected []any) map[string]any {
	if packages == nil {
		packages = []any{}
	}
	if rejected == nil {
		rejected = []any{}
	}
	return map[string]any{
		"kind": "installed_vpk_scan", "directory": managedVPKDirectory,
		"packages": packages, "rejected": rejected,
		"discovered_count": len(packages) + len(rejected),
		"valid_count":      len(packages), "rejected_count": len(rejected),
	}
}

func scannedPackageRejection(name string, err error) map[string]any {
	reason := "validation failed"
	if err != nil {
		reason = strings.ToValidUTF8(err.Error(), "?")
		if len(reason) > 512 {
			reason = reason[:512]
		}
	}
	return map[string]any{"name": filepath.Base(name), "reason": reason}
}
