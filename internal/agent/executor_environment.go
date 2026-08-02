package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) inspectRuntimeEnvironment(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	progress(10, "checking system dependencies")
	result := map[string]any{"system_dependencies": systemDependencyStatus(), "steamcmd_source": "unconfigured", "steamcmd_status": "unconfigured"}
	if e.runtimeEnvironment == nil {
		progress(100, "runtime environment inspected")
		return result, nil
	}
	if requested, ok := job.Payload["installations"].([]any); ok {
		if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
			for _, value := range requested {
				item, ok := value.(map[string]any)
				if !ok {
					continue
				}
				id, relative := payloadString(item, "id"), payloadString(item, "path")
				if id != "" && relative != "" {
					state.Installations[id] = runtimeGameRecord{RootIndex: intFromPayload(item, "root_index"), RelativePath: relative}
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	state := e.runtimeEnvironment.snapshot()
	if state.SteamCMDPath != "" {
		progress(35, "checking configured SteamCMD")
		version, err := validateSteamCMDPath(ctx, state.SteamCMDPath, e.paths.Roots())
		result["steamcmd_path"], result["steamcmd_source"] = state.SteamCMDPath, defaultRuntimeSource(state.SteamCMDSource)
		if err != nil {
			result["steamcmd_status"], result["steamcmd_error"] = "error", err.Error()
		} else {
			result["steamcmd_status"], result["steamcmd_version"] = "ready", version
			e.replaceSteamClient(state.SteamCMDPath)
		}
	}
	installations := make([]map[string]any, 0, len(state.Installations))
	for id, record := range state.Installations {
		status, buildID, message := e.inspectGamePath(record.RootIndex, record.RelativePath)
		installations = append(installations, map[string]any{"id": id, "status": status, "build_id": buildID, "error": message})
	}
	result["installations"] = installations
	progress(100, "runtime environment inspected")
	return result, nil
}

func (e *Executor) configureSteamCMD(ctx context.Context, job domain.Job, source string, progress ProgressFunc) (map[string]any, error) {
	if e.runtimeEnvironment == nil {
		return nil, errors.New("runtime environment state is unavailable")
	}
	path := strings.TrimSpace(payloadString(job.Payload, "path"))
	progress(20, "validating SteamCMD path")
	version, err := validateSteamCMDPath(ctx, path, e.paths.Roots())
	if err != nil {
		return nil, err
	}
	if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
		state.SteamCMDPath, state.SteamCMDSource = filepath.Clean(path), source
	}); err != nil {
		return nil, err
	}
	e.replaceSteamClient(path)
	progress(100, "SteamCMD path activated")
	return map[string]any{"steamcmd_path": filepath.Clean(path), "steamcmd_source": source, "steamcmd_version": version}, nil
}

func (e *Executor) clearSteamCMD(progress ProgressFunc) (map[string]any, error) {
	if e.runtimeEnvironment == nil {
		return nil, errors.New("runtime environment state is unavailable")
	}
	if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
		state.SteamCMDPath, state.SteamCMDSource = "", ""
	}); err != nil {
		return nil, err
	}
	e.replaceSteamClient("")
	progress(100, "SteamCMD configuration cleared")
	return map[string]any{"steamcmd_source": "unconfigured"}, nil
}

func (e *Executor) installManagedSteamCMD(ctx context.Context, progress ProgressFunc) (map[string]any, error) {
	if e.runtimeEnvironment == nil {
		return nil, errors.New("runtime environment state is unavailable")
	}
	progress(5, "downloading managed SteamCMD")
	path, version, err := installManagedSteamCMD(ctx, e.config, e.paths.Roots())
	if err != nil {
		return nil, err
	}
	if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
		state.SteamCMDPath, state.SteamCMDSource = path, "managed"
	}); err != nil {
		return nil, err
	}
	e.replaceSteamClient(path)
	progress(100, "managed SteamCMD installed")
	return map[string]any{"steamcmd_path": path, "steamcmd_source": "managed", "steamcmd_version": version}, nil
}

func (e *Executor) gameInstallationAction(ctx context.Context, job domain.Job, action string, progress ProgressFunc) (map[string]any, error) {
	if job.InstallationID == "" {
		return nil, errors.New("game installation id is required")
	}
	rootIndex := intFromPayload(job.Payload, "root_index")
	relative := strings.TrimSpace(payloadString(job.Payload, "path"))
	if relative == "" {
		return nil, errors.New("game installation path is required")
	}
	path, err := e.paths.Resolve(rootIndex, relative)
	if err != nil {
		return nil, err
	}
	if err := rejectLinkedAncestors(path); err != nil {
		return nil, err
	}
	if action != "game.install" && action != "game.import" && e.runtimeEnvironment != nil {
		registered, ok := e.runtimeEnvironment.snapshot().Installations[job.InstallationID]
		if !ok || registered.RootIndex != rootIndex || registered.RelativePath != relative {
			return nil, errors.New("game installation is not registered on this Agent")
		}
	}
	if action == "game.quarantine" {
		if e.runtimeEnvironment == nil {
			return nil, errors.New("runtime environment state is unavailable")
		}
		if err := rejectLinkedTree(path); err != nil {
			return nil, err
		}
		entry := job.InstallationID + "-" + time.Now().UTC().Format("20060102T150405Z")
		target, err := PrepareQuarantineTarget(e.paths, rootIndex, entry)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, target); err != nil {
			return nil, err
		}
		if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) { delete(state.Installations, job.InstallationID) }); err != nil {
			if rollbackErr := os.Rename(target, path); rollbackErr != nil {
				return nil, fmt.Errorf("persist quarantine state: %w; rollback move: %v", err, rollbackErr)
			}
			return nil, err
		}
		progress(100, "game installation moved to quarantine")
		return map[string]any{"quarantine_entry": entry, "root_index": rootIndex}, nil
	}

	steam := e.steamClient()
	if action != "game.import" {
		if e.runtimeEnvironment == nil {
			return nil, errors.New("runtime environment state is unavailable")
		}
		state := e.runtimeEnvironment.snapshot()
		if state.SteamCMDPath == "" {
			return nil, errors.New("SteamCMD is not configured")
		}
		if _, err := validateSteamCMDPath(ctx, state.SteamCMDPath, e.paths.Roots()); err != nil {
			return nil, err
		}
	}
	switch action {
	case "game.install":
		if entries, readErr := os.ReadDir(path); readErr == nil && len(entries) > 0 {
			return nil, errors.New("game installation directory is not empty; use import")
		}
		progress(5, "installing L4D2 dedicated server")
		err = steam.Install(ctx, path, L4D2DedicatedAppID, false, progress)
	case "game.validate", "game.reinstall":
		progress(5, "validating L4D2 dedicated server")
		err = steam.Install(ctx, path, L4D2DedicatedAppID, true, progress)
	case "game.update":
		progress(5, "updating L4D2 dedicated server")
		err = steam.Install(ctx, path, L4D2DedicatedAppID, false, progress)
	case "game.import":
		progress(40, "validating existing L4D2 installation")
	default:
		return nil, fmt.Errorf("unsupported game installation action %q", action)
	}
	if err != nil {
		return nil, err
	}
	status, buildID, message := e.inspectGamePath(rootIndex, relative)
	if status != "ready" {
		return nil, errors.New(message)
	}
	if e.runtimeEnvironment != nil {
		if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
			state.Installations[job.InstallationID] = runtimeGameRecord{RootIndex: rootIndex, RelativePath: relative}
		}); err != nil {
			return nil, err
		}
	}
	progress(100, "game installation is ready")
	return map[string]any{"path": path, "root_index": rootIndex, "build_id": buildID, "app_id": L4D2DedicatedAppID}, nil
}

func (e *Executor) inspectGamePath(rootIndex int, relative string) (status, buildID, message string) {
	path, err := e.paths.Resolve(rootIndex, relative)
	if err != nil {
		return "degraded", "", err.Error()
	}
	if err := rejectLinkedPath(path); err != nil {
		return "degraded", "", err.Error()
	}
	executable, err := ResolveWithin(path, defaultServerExecutable())
	if err != nil {
		return "degraded", "", err.Error()
	}
	if info, err := os.Lstat(executable); err != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) {
		return "degraded", "", "path does not contain a safe L4D2 dedicated server executable"
	}
	gameInfo, err := ResolveWithin(path, "left4dead2/gameinfo.txt")
	if err != nil {
		return "degraded", "", err.Error()
	}
	if info, err := os.Lstat(gameInfo); err != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) {
		return "degraded", "", "path does not contain left4dead2/gameinfo.txt"
	}
	if steam := e.steamClient(); steam != nil {
		if value, _, err := steam.InstalledBuildID(path, L4D2DedicatedAppID); err == nil {
			buildID = value
		}
	}
	return "ready", buildID, ""
}

func defaultRuntimeSource(value string) string {
	if value == "managed" || value == "custom" {
		return value
	}
	return "custom"
}

func intFromPayload(payload map[string]any, key string) int {
	switch value := payload[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}
