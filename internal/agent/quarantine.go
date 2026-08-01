package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const defaultQuarantineRetention = 7 * 24 * time.Hour

var quarantineEntryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+-(\d{8}T\d{6}Z)$`)

// PrepareQuarantineTarget returns a previously unused, direct child of the
// Agent-owned quarantine directory. Callers must still use an atomic rename to
// move the instance into the returned path.
func PrepareQuarantineTarget(paths *PathGuard, rootIndex int, entryName string) (string, error) {
	quarantine, err := resolveQuarantineDirectory(paths, rootIndex, true)
	if err != nil {
		return "", err
	}
	target, err := resolveQuarantineEntry(paths, rootIndex, quarantine, entryName)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(target); err == nil {
		return "", errors.New("quarantine entry already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return target, nil
}

// PurgeQuarantine permanently removes exactly one Agent-created quarantine
// entry. It accepts only a configured root index and a strict basename; it
// rejects links and reparse points anywhere in the tree before removal.
func PurgeQuarantine(paths *PathGuard, rootIndex int, entryName string) (bool, error) {
	return purgeQuarantineAt(paths, rootIndex, entryName, time.Now().UTC())
}

func purgeQuarantineAt(paths *PathGuard, rootIndex int, entryName string, now time.Time) (bool, error) {
	matches := quarantineEntryPattern.FindStringSubmatch(entryName)
	if len(matches) != 2 {
		return false, errors.New("invalid quarantine entry name")
	}
	quarantinedAt, err := time.Parse("20060102T150405Z", matches[1])
	if err != nil {
		return false, errors.New("invalid quarantine entry timestamp")
	}
	if now.UTC().Before(quarantinedAt.Add(defaultQuarantineRetention)) {
		return false, errors.New("quarantine entry has not completed its seven-day minimum retention")
	}
	quarantine, err := resolveQuarantineDirectory(paths, rootIndex, false)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	target, err := resolveQuarantineEntry(paths, rootIndex, quarantine, entryName)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || isReparseOrSymlink(info) {
		return false, errors.New("quarantine entry is not a plain directory")
	}
	if err := rejectLinkedTree(target); err != nil {
		return false, err
	}
	// Re-resolve and re-inspect immediately before the destructive operation so
	// a replaced top-level entry cannot redirect removal outside quarantine.
	validated, err := resolveQuarantineEntry(paths, rootIndex, quarantine, entryName)
	if err != nil || !samePath(validated, target) {
		if err == nil {
			err = ErrPathEscape
		}
		return false, err
	}
	info, err = os.Lstat(validated)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() || isReparseOrSymlink(info) {
		return false, errors.New("quarantine entry changed during validation")
	}
	if err := os.RemoveAll(validated); err != nil {
		return false, err
	}
	return true, nil
}

func resolveQuarantineDirectory(paths *PathGuard, rootIndex int, create bool) (string, error) {
	if paths == nil {
		return "", errors.New("path guard is required")
	}
	roots := paths.Roots()
	if rootIndex < 0 || rootIndex >= len(roots) {
		return "", errors.New("invalid root index")
	}
	raw := filepath.Join(roots[rootIndex], ".quarantine")
	if create {
		if err := os.MkdirAll(raw, 0o750); err != nil {
			return "", err
		}
	}
	info, err := os.Lstat(raw)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || isReparseOrSymlink(info) {
		return "", errors.New("quarantine root is not a plain directory")
	}
	resolved, err := paths.Resolve(rootIndex, ".quarantine")
	if err != nil {
		return "", err
	}
	if !samePath(resolved, raw) {
		return "", ErrPathEscape
	}
	return resolved, nil
}

func resolveQuarantineEntry(paths *PathGuard, rootIndex int, quarantine, entryName string) (string, error) {
	if len(entryName) > 200 || filepath.Base(entryName) != entryName || quarantineEntryPattern.FindStringSubmatch(entryName) == nil {
		return "", errors.New("invalid quarantine entry name")
	}
	target, err := paths.Resolve(rootIndex, filepath.Join(".quarantine", entryName))
	if err != nil {
		return "", err
	}
	if !samePath(filepath.Dir(target), quarantine) {
		return "", ErrPathEscape
	}
	return target, nil
}

func rejectLinkedTree(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if isReparseOrSymlink(info) {
			return fmt.Errorf("quarantine entry contains a link or reparse point at %q", filepath.Base(path))
		}
		return nil
	})
}
