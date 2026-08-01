package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var instanceLogNamePattern = regexp.MustCompile(`^([A-Za-z0-9._-]+)\.log(?:\.([1-9]|1[0-9]))?$`)

// CleanupLogs enforces age retention even for instances whose log is no
// longer opened or written. The current file of an active process is kept;
// its closed rotated segments remain safe to prune.
func (m *ProcessManager) CleanupLogs(now time.Time, maxAge time.Duration) (int, error) {
	if m == nil || maxAge <= 0 {
		return 0, errors.New("process manager and positive log retention are required")
	}
	entries, err := os.ReadDir(m.logDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	m.mu.RLock()
	active := make(map[string]bool, len(m.items))
	for instanceID, process := range m.items {
		active[safeLogName(instanceID)] = process != nil && (process.launching || process.cmd != nil)
	}
	m.mu.RUnlock()

	cutoff := now.Add(-maxAge)
	removed := 0
	var cleanupErrors []error
	for _, entry := range entries {
		matches := instanceLogNamePattern.FindStringSubmatch(entry.Name())
		if len(matches) != 3 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect instance log %q: %w", entry.Name(), err))
			continue
		}
		if !info.Mode().IsRegular() || isReparseOrSymlink(info) || !info.ModTime().Before(cutoff) {
			continue
		}
		// matches[2] is empty only for the current segment.
		if matches[2] == "" && active[matches[1]] {
			continue
		}
		path, err := ResolveWithin(m.logDir, entry.Name())
		if err != nil || !samePath(filepath.Dir(path), m.logDir) {
			if err == nil {
				err = ErrPathEscape
			}
			cleanupErrors = append(cleanupErrors, fmt.Errorf("validate instance log %q: %w", entry.Name(), err))
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove instance log %q: %w", entry.Name(), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(cleanupErrors...)
}
