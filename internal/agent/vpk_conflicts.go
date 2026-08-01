package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxVPKConflictCandidates = 512
	maxVPKConflictEntries    = 500000
	maxVPKConflictReport     = 16
)

type vpkInstallCandidate struct {
	Source         string
	TargetRelative string
}

func isVPKPackageKind(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	return kind == "vpk" || kind == "vpk_url" || kind == "workshop"
}

// validateVPKContentConflicts compares the virtual files contained in one or
// more candidate VPKs with every enabled, direct-child VPK in the instance
// addons directory. A replacement may overlap the exact VPK it replaces, but
// not any other package. Source paths are fully validated before comparison.
func validateVPKContentConflicts(instancePath string, candidates []vpkInstallCandidate) error {
	if len(candidates) == 0 {
		return errors.New("at least one VPK conflict candidate is required")
	}
	if len(candidates) > maxVPKConflictCandidates {
		return errors.New("VPK conflict candidate count exceeds limit")
	}
	addonsRoot, err := ResolveWithin(instancePath, filepath.Join("left4dead2", "addons"))
	if err != nil {
		return err
	}
	excludedTargets := make([]string, 0, len(candidates))
	seenTargets := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.Source) == "" {
			return errors.New("VPK conflict candidate source is required")
		}
		target, err := ResolveWithin(instancePath, filepath.FromSlash(candidate.TargetRelative))
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(addonsRoot, target)
		if err != nil || filepath.Dir(relative) != "." || !strings.EqualFold(filepath.Ext(relative), ".vpk") {
			return errors.New("VPK conflict target is outside the enabled addons directory")
		}
		key := normalizedVPKFilesystemPath(target)
		if _, exists := seenTargets[key]; exists {
			return fmt.Errorf("multiple VPK candidates target %q", filepath.Base(target))
		}
		seenTargets[key] = struct{}{}
		excludedTargets = append(excludedTargets, target)
	}

	// The first package owning a virtual path is enough for comparison. Existing
	// legacy conflicts are left untouched, but any new or restored VPK that
	// overlaps either owner is rejected.
	owners := make(map[string]string)
	totalEntries := 0
	entries, err := os.ReadDir(addonsRoot)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if len(entries) > maxVPKConflictCandidates*2 {
			return errors.New("enabled VPK count exceeds conflict-scan limit")
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".vpk") {
				continue
			}
			path, err := ResolveWithin(addonsRoot, entry.Name())
			if err != nil {
				return err
			}
			excluded := false
			for _, target := range excludedTargets {
				if samePath(path, target) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || isReparseOrSymlink(info) {
				return fmt.Errorf("cannot inspect enabled VPK %q because it is not a trusted regular file", entry.Name())
			}
			_, _, contentPaths, err := validateVPKDetailed(path)
			if err != nil {
				return fmt.Errorf("cannot inspect enabled VPK %q: %w", entry.Name(), err)
			}
			totalEntries += len(contentPaths)
			if totalEntries > maxVPKConflictEntries {
				return errors.New("enabled VPK content count exceeds conflict-scan limit")
			}
			for _, contentPath := range contentPaths {
				key := strings.ToLower(filepath.ToSlash(contentPath))
				if _, exists := owners[key]; !exists {
					owners[key] = entry.Name()
				}
			}
		}
	}

	conflicts := make([]string, 0)
	for _, candidate := range candidates {
		_, _, contentPaths, err := validateVPKDetailed(candidate.Source)
		if err != nil {
			return fmt.Errorf("candidate VPK %q failed validation: %w", filepath.Base(candidate.TargetRelative), err)
		}
		totalEntries += len(contentPaths)
		if totalEntries > maxVPKConflictEntries {
			return errors.New("VPK content count exceeds conflict-scan limit")
		}
		candidateName := filepath.Base(filepath.FromSlash(candidate.TargetRelative))
		for _, contentPath := range contentPaths {
			key := strings.ToLower(filepath.ToSlash(contentPath))
			if owner, exists := owners[key]; exists {
				if len(conflicts) < maxVPKConflictReport {
					conflicts = append(conflicts, fmt.Sprintf("%s (%s)", contentPath, owner))
				}
				continue
			}
			owners[key] = candidateName
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	return fmt.Errorf("VPK content conflicts detected: %s", strings.Join(conflicts, ", "))
}

func normalizedVPKFilesystemPath(path string) string {
	cleaned := filepath.Clean(path)
	// Instance registry semantics are case-insensitive on Windows. Keeping this
	// normalization case-insensitive everywhere also matches Source's virtual
	// VPK path lookup and prevents case-only package target ambiguity on Linux.
	return strings.ToLower(filepath.ToSlash(cleaned))
}
