package portableupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func RunWorker(root, jobID string) error {
	// Let the HTTP response flush or the foreground CLI process exit before the
	// worker stops the service and replaces the original executable on Windows.
	time.Sleep(2 * time.Second)
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	manager := &Manager{Root: root, ConfigPath: filepath.Join(root, "data", "config", "control.json"),
		DatabasePath: filepath.Join(root, "data", "database", "l4d2panel.sqlite3"), ExecutablePath: filepath.Join(root, "l4d2panel-control.exe")}
	state, err := manager.State(jobID)
	if err != nil {
		return err
	}
	manager.CurrentVersion = state.CurrentVersion
	return runWorker(manager, jobID)
}

func runWorker(manager *Manager, jobID string) error {
	fail := func(cause error) error {
		rollbackErr := manager.rollback(jobID)
		now := time.Now().UTC()
		_ = manager.updateState(jobID, "failed", "rolling_back", func(value *State) {
			value.Error = manager.sanitizedError(cause)
			value.CompletedAt = &now
			if rollbackErr == nil {
				value.Rollback = "succeeded"
			} else {
				value.Rollback = "failed"
				value.Error = manager.sanitizedError(fmt.Errorf("%v; rollback failed: %v", cause, rollbackErr))
			}
		})
		manager.releaseActive(jobID)
		_ = manager.rotateSnapshots(3)
		if rollbackErr == nil {
			_ = os.Remove(filepath.Join(manager.jobDir(jobID), "package.zip"))
			_ = os.RemoveAll(filepath.Join(manager.jobDir(jobID), "staging"))
		}
		if rollbackErr != nil {
			return fmt.Errorf("update failed: %w; rollback failed: %v", cause, rollbackErr)
		}
		return cause
	}
	if err := manager.updateState(jobID, "running", "stopping", nil); err != nil {
		return err
	}
	if err := manager.execute(manager.ExecutablePath, "service", "stop", "--config", manager.ConfigPath); err != nil {
		return fail(fmt.Errorf("stop Control service: %w", err))
	}
	if err := manager.updateState(jobID, "running", "backing_up", nil); err != nil {
		return fail(err)
	}
	if err := manager.snapshot(jobID); err != nil {
		return fail(fmt.Errorf("create upgrade snapshot: %w", err))
	}
	if err := manager.updateState(jobID, "running", "installing", nil); err != nil {
		return fail(err)
	}
	if err := manager.installStaged(jobID); err != nil {
		return fail(fmt.Errorf("install staged package: %w", err))
	}
	if err := manager.updateState(jobID, "running", "migrating", nil); err != nil {
		return fail(err)
	}
	if err := manager.execute(manager.ExecutablePath, "doctor", "--config", manager.ConfigPath); err != nil {
		return fail(fmt.Errorf("new Control doctor failed: %w", err))
	}
	if err := manager.execute(manager.ExecutablePath, "migrate", "--config", manager.ConfigPath); err != nil {
		return fail(fmt.Errorf("database migration failed: %w", err))
	}
	if err := manager.updateState(jobID, "running", "starting", nil); err != nil {
		return fail(err)
	}
	if err := manager.execute(manager.ExecutablePath, "service", "start", "--config", manager.ConfigPath); err != nil {
		return fail(fmt.Errorf("start new Control service: %w", err))
	}
	if err := manager.updateState(jobID, "running", "health_check", nil); err != nil {
		return fail(err)
	}
	if err := manager.checkReady(90 * time.Second); err != nil {
		return fail(err)
	}
	now := time.Now().UTC()
	if err := manager.updateState(jobID, "succeeded", "health_check", func(value *State) {
		value.CompletedAt, value.Rollback, value.Error = &now, "not_required", ""
	}); err != nil {
		return err
	}
	manager.releaseActive(jobID)
	_ = os.Remove(filepath.Join(manager.jobDir(jobID), "package.zip"))
	_ = os.RemoveAll(filepath.Join(manager.jobDir(jobID), "staging"))
	return manager.rotateSnapshots(3)
}

func (m *Manager) snapshot(jobID string) error {
	snapshot := m.snapshotDir(jobID)
	if err := os.MkdirAll(filepath.Join(snapshot, "program"), 0o700); err != nil {
		return err
	}
	manifest, err := loadManifest(filepath.Join(m.Root, ManifestName))
	if err != nil {
		return err
	}
	for _, relative := range SortedManifestPaths(manifest) {
		source := filepath.Join(m.Root, filepath.FromSlash(relative))
		target := filepath.Join(snapshot, "program", filepath.FromSlash(relative))
		if err := copyRegularFile(source, target, 0o600); err != nil {
			return err
		}
	}
	if err := copyRegularFile(m.ConfigPath, filepath.Join(snapshot, "control.json"), 0o600); err != nil {
		return err
	}
	if err := m.execute(m.ExecutablePath, "backup-database", "--config", m.ConfigPath, "--output", filepath.Join(snapshot, "l4d2panel.sqlite3")); err != nil {
		return fmt.Errorf("create consistent SQLite backup: %w", err)
	}
	return os.WriteFile(filepath.Join(snapshot, "snapshot.complete"), []byte("complete\n"), 0o600)
}

func (m *Manager) installStaged(jobID string) error {
	staging := filepath.Join(m.jobDir(jobID), "staging")
	oldManifest, err := loadManifest(filepath.Join(m.Root, ManifestName))
	if err != nil {
		return err
	}
	newManifest, err := loadManifest(filepath.Join(staging, ManifestName))
	if err != nil {
		return err
	}
	newFiles := make(map[string]struct{}, len(newManifest.Files))
	for _, item := range newManifest.Files {
		newFiles[item.Path] = struct{}{}
		if err := copyRegularFile(filepath.Join(staging, filepath.FromSlash(item.Path)), filepath.Join(m.Root, filepath.FromSlash(item.Path)), 0o700); err != nil {
			return err
		}
	}
	for _, item := range oldManifest.Files {
		if _, retained := newFiles[item.Path]; !retained {
			if err := os.Remove(filepath.Join(m.Root, filepath.FromSlash(item.Path))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return copyRegularFile(filepath.Join(staging, ManifestName), filepath.Join(m.Root, ManifestName), 0o600)
}

func (m *Manager) rollback(jobID string) error {
	snapshot := m.snapshotDir(jobID)
	if _, err := os.Stat(filepath.Join(snapshot, "snapshot.complete")); err != nil {
		if startErr := m.execute(m.ExecutablePath, "service", "start", "--config", m.ConfigPath); startErr != nil {
			return fmt.Errorf("upgrade snapshot was not completed and old service could not restart: %w", startErr)
		}
		return m.checkReady(90 * time.Second)
	}
	_ = m.execute(m.ExecutablePath, "service", "stop", "--config", m.ConfigPath)
	currentManifest, _ := loadManifest(filepath.Join(m.Root, ManifestName))
	oldManifest, err := loadManifest(filepath.Join(snapshot, "program", ManifestName))
	if err != nil {
		return err
	}
	oldFiles := make(map[string]struct{}, len(oldManifest.Files))
	for _, item := range oldManifest.Files {
		oldFiles[item.Path] = struct{}{}
		if err := copyRegularFile(filepath.Join(snapshot, "program", filepath.FromSlash(item.Path)), filepath.Join(m.Root, filepath.FromSlash(item.Path)), 0o700); err != nil {
			return err
		}
	}
	for _, item := range currentManifest.Files {
		if _, retained := oldFiles[item.Path]; !retained {
			_ = os.Remove(filepath.Join(m.Root, filepath.FromSlash(item.Path)))
		}
	}
	if err := copyRegularFile(filepath.Join(snapshot, "program", ManifestName), filepath.Join(m.Root, ManifestName), 0o600); err != nil {
		return err
	}
	if err := copyRegularFile(filepath.Join(snapshot, "control.json"), m.ConfigPath, 0o600); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(m.DatabasePath + suffix)
		source := filepath.Join(snapshot, "l4d2panel.sqlite3"+suffix)
		if _, err := os.Stat(source); err == nil {
			if err := copyRegularFile(source, m.DatabasePath+suffix, 0o600); err != nil {
				return err
			}
		}
	}
	if err := m.execute(m.ExecutablePath, "service", "start", "--config", m.ConfigPath); err != nil {
		return err
	}
	return m.checkReady(90 * time.Second)
}

func (m *Manager) execute(executable string, args ...string) error {
	if m.command != nil {
		return m.command(executable, args...)
	}
	return runCommand(executable, args...)
}

func (m *Manager) checkReady(timeout time.Duration) error {
	if m.ready != nil {
		return m.ready(timeout)
	}
	return waitReady(timeout)
}

func (m *Manager) snapshotDir(jobID string) string {
	return filepath.Join(m.Root, "data", "backups", "upgrades", jobID)
}

func (m *Manager) rotateSnapshots(keep int) error {
	root := filepath.Join(m.Root, "data", "backups", "upgrades")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		name string
		at   time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			candidates = append(candidates, candidate{name: entry.Name(), at: info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].at.After(candidates[j].at) })
	if len(candidates) <= keep {
		return nil
	}
	for _, item := range candidates[keep:] {
		if err := os.RemoveAll(filepath.Join(root, item.name)); err != nil {
			return err
		}
	}
	return nil
}

func loadManifest(filename string) (Manifest, error) {
	f, err := os.Open(filename)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.FormatVersion != portableFormat || manifest.Product != portableProduct || manifest.Platform != portablePlatform || manifest.Architecture != portableArchitecture {
		return Manifest{}, errors.New("installed portable manifest identity is invalid")
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, item := range manifest.Files {
		clean, err := safeArchivePath(item.Path)
		folded := strings.ToLower(clean)
		if err != nil || clean != item.Path || clean == ManifestName {
			return Manifest{}, fmt.Errorf("installed portable manifest has unsafe path %q", item.Path)
		}
		if _, exists := seen[folded]; exists {
			return Manifest{}, fmt.Errorf("installed portable manifest repeats path %q", item.Path)
		}
		seen[folded] = struct{}{}
	}
	return manifest, nil
}

func waitReady(timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://127.0.0.1:19090/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("Control readiness check timed out")
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(err.Error(), "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}

var setupKeyInDiagnostic = regexp.MustCompile(`l4d2p_setup_[A-Za-z0-9_-]{43}`)

func (m *Manager) sanitizedError(err error) string {
	message := sanitizeError(err)
	for _, sensitivePath := range []string{m.ConfigPath, m.DatabasePath, m.ExecutablePath, m.Root} {
		message = replaceFold(message, sensitivePath, "[portable-path]")
		message = replaceFold(message, filepath.ToSlash(sensitivePath), "[portable-path]")
	}
	return setupKeyInDiagnostic.ReplaceAllString(message, "[setup-key-redacted]")
}

func replaceFold(value, old, replacement string) string {
	if old == "" {
		return value
	}
	for {
		index := strings.Index(strings.ToLower(value), strings.ToLower(old))
		if index < 0 {
			return value
		}
		value = value[:index] + replacement + value[index+len(old):]
	}
}
