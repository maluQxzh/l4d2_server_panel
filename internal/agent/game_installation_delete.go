package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) deleteGameInstallation(job domain.Job, progress ProgressFunc) (map[string]any, error) {
	if e.runtimeEnvironment == nil {
		return nil, errors.New("runtime environment state is unavailable")
	}
	installationID := strings.TrimSpace(job.InstallationID)
	requestedID := strings.TrimSpace(exactPayloadString(job.Payload, "installation_id"))
	requestedPath := strings.TrimSpace(exactPayloadString(job.Payload, "path"))
	mode := strings.ToLower(strings.TrimSpace(exactPayloadString(job.Payload, "mode")))
	if installationID == "" || requestedID != installationID {
		return nil, errors.New("game installation id does not match the deletion request")
	}
	if mode != "record" && mode != "files" {
		return nil, errors.New("game installation deletion mode must be record or files")
	}
	record, ok := e.runtimeEnvironment.snapshot().Installations[installationID]
	if !ok || requestedPath == "" || !samePath(record.Path, requestedPath) {
		return nil, errors.New("game installation is not registered at the requested path")
	}
	for _, instance := range e.registry.List() {
		instancePath, err := resolveInstanceRecordPath(e.paths, instance)
		if err == nil && isWithin(record.Path, instancePath) {
			return nil, fmt.Errorf("game installation is still registered to instance %q", instance.ID)
		}
	}
	if mode == "files" {
		progress(10, "validating game installation deletion target")
		if err := e.removeRegisteredGameInstallation(installationID, record.Path); err != nil {
			return nil, err
		}
	}
	if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
		delete(state.Installations, installationID)
	}); err != nil {
		return nil, err
	}
	progress(100, "game installation deleted")
	return map[string]any{
		"installation_id": installationID,
		"path":            requestedPath,
		"files_deleted":   mode == "files",
	}, nil
}

func (e *Executor) removeRegisteredGameInstallation(installationID, value string) error {
	path, err := e.paths.ResolveInstallation(0, value)
	if err != nil {
		return err
	}
	if !samePath(path, value) {
		return ErrPathEscape
	}
	parent := filepath.Dir(path)
	if samePath(parent, path) {
		return errors.New("refusing to delete a filesystem root")
	}
	protected := append(e.paths.Roots(), e.config.DataDir)
	state := e.runtimeEnvironment.snapshot()
	if state.SteamCMDPath != "" {
		protected = append(protected, state.SteamCMDPath)
	}
	for id, installation := range state.Installations {
		if id != installationID && strings.TrimSpace(installation.Path) != "" {
			protected = append(protected, installation.Path)
		}
	}
	if executable, executableErr := os.Executable(); executableErr == nil {
		protected = append(protected, filepath.Dir(executable))
	}
	for _, candidate := range protected {
		if strings.TrimSpace(candidate) != "" && isWithin(path, candidate) {
			return errors.New("game installation deletion target contains an Agent-managed root")
		}
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || isReparseOrSymlink(info) {
		return errors.New("game installation deletion target is not a plain directory")
	}
	if err := rejectLinkedTree(path); err != nil {
		return fmt.Errorf("game installation deletion target is unsafe: %w", err)
	}
	if err := rejectLinkedAncestors(path); err != nil {
		return err
	}
	resolved, err := e.paths.ResolveInstallation(0, value)
	if err != nil || !samePath(resolved, path) {
		if err == nil {
			err = ErrPathEscape
		}
		return err
	}
	temporaryName, err := managedTempName()
	if err != nil {
		return err
	}
	deletingPath := filepath.Join(parent, ".l4d2panel-delete-"+strings.TrimPrefix(temporaryName, ".managed-"))
	if _, err := os.Lstat(deletingPath); err == nil {
		return errors.New("temporary deletion target already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(path, deletingPath); err != nil {
		return fmt.Errorf("isolate game installation for deletion: %w", err)
	}
	if err := rejectLinkedTree(deletingPath); err != nil {
		_ = os.Rename(deletingPath, path)
		return fmt.Errorf("isolated game installation deletion target is unsafe: %w", err)
	}
	if err := os.RemoveAll(deletingPath); err != nil {
		_ = os.Rename(deletingPath, path)
		return fmt.Errorf("delete game installation files: %w", err)
	}
	return nil
}
