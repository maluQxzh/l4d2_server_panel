package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	smartPluginGeneratedConfigDirectory       = "left4dead2/cfg/sourcemod"
	smartPluginGeneratedConfigPollInterval    = 3 * time.Second
	smartPluginGeneratedConfigMaxPollAttempts = 5
)

type loadedSmartPluginState struct {
	state smartPluginState
	root  string
}

// reconcileSmartPluginGeneratedConfigs adopts only direct cfg/sourcemod CFGs
// whose basename uniquely matches an enabled, managed SMX entrypoint. This is
// deliberately narrower than general file scanning: candidate paths are not
// caller supplied, ambiguous matches and server-registered paths are ignored,
// and every candidate is opened through the existing no-follow instance-root
// operations before it enters a deletion inventory.
func (e *Executor) reconcileSmartPluginGeneratedConfigs(instanceID string, registeredPaths map[string]string, targetPackageID string) ([]map[string]any, error) {
	record, ok := e.registry.Get(instanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	states, err := e.loadSmartPluginStates(instanceID)
	if err != nil || len(states) == 0 {
		return nil, err
	}

	ownedPaths := make(map[string]bool)
	for path := range registeredPaths {
		ownedPaths[strings.ToLower(filepath.ToSlash(strings.TrimSpace(path)))] = true
	}
	ownersByBase := make(map[string]map[int]bool)
	for index := range states {
		for _, file := range states[index].state.Files {
			ownedPaths[strings.ToLower(file.TargetPath)] = true
			base, eligible := generatedConfigSMXBase(states[index].state.Enabled, file)
			if !eligible {
				continue
			}
			if ownersByBase[base] == nil {
				ownersByBase[base] = make(map[int]bool)
			}
			ownersByBase[base][index] = true
		}
	}

	entries, err := listManagedDirectoryNoFollow(instancePath, smartPluginGeneratedConfigDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > maxSmartPluginFiles {
		return nil, errors.New("SourceMod generated config directory contains too many entries")
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].Path) < strings.ToLower(entries[j].Path) })
	candidateCounts := make(map[string]int)
	for _, entry := range entries {
		if !entry.IsDir && strings.EqualFold(filepath.Ext(entry.Name), ".cfg") {
			candidateCounts[strings.ToLower(entry.Path)]++
		}
	}
	additions := make(map[int][]smartPluginStateFile)
	for _, entry := range entries {
		pathKey := strings.ToLower(entry.Path)
		if entry.IsDir || !strings.EqualFold(filepath.Ext(entry.Name), ".cfg") || candidateCounts[pathKey] != 1 || ownedPaths[pathKey] {
			continue
		}
		base := strings.ToLower(strings.TrimSuffix(entry.Name, filepath.Ext(entry.Name)))
		owners := ownersByBase[base]
		if len(owners) != 1 {
			continue
		}
		owner := -1
		for index := range owners {
			owner = index
		}
		if owner < 0 || (targetPackageID != "" && states[owner].state.PackageID != targetPackageID) || len(states[owner].state.Files)+len(additions[owner]) >= maxSmartPluginFiles {
			continue
		}
		stableSHA, stableSize, hashErr := hashRegularFileWithinNoFollow(instancePath, entry.Path, maxManagedFileSize)
		raw, readErr := readRegularFileWithinNoFollow(instancePath, entry.Path, maxManagedFileSize)
		digest := sha256.Sum256(raw)
		sha := hex.EncodeToString(digest[:])
		if hashErr != nil || readErr != nil || int64(len(raw)) != stableSize || !strings.EqualFold(sha, stableSHA) || bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
			continue
		}
		additions[owner] = append(additions[owner], smartPluginStateFile{
			ID: smartStableID("generated-config", entry.Path), TargetPath: entry.Path,
			SHA256: sha, Size: int64(len(raw)), Entrypoint: false, Generated: true,
		})
		ownedPaths[pathKey] = true
	}

	updates := make([]map[string]any, 0, len(additions))
	stateIndexes := make([]int, 0, len(additions))
	for index := range additions {
		stateIndexes = append(stateIndexes, index)
	}
	sort.Slice(stateIndexes, func(i, j int) bool {
		return states[stateIndexes[i]].state.PackageID < states[stateIndexes[j]].state.PackageID
	})
	for _, index := range stateIndexes {
		files := additions[index]
		sort.Slice(files, func(i, j int) bool {
			return strings.ToLower(files[i].TargetPath) < strings.ToLower(files[j].TargetPath)
		})
		state := states[index].state
		state.Files = append(state.Files, files...)
		state.AggregateSHA = smartPluginAggregateSHA(state.Files)
		if err := e.updateSmartPluginManifest(states[index].root, state); err != nil {
			if e.logger != nil {
				e.logger.Printf("instance %s reconcile generated SourceMod config for smart package %s: %v", instanceID, state.PackageID, err)
			}
			continue
		}
		fileResults := make([]map[string]any, 0, len(files))
		for _, file := range files {
			fileResults = append(fileResults, map[string]any{
				"path": file.TargetPath, "sha256": file.SHA256, "size": file.Size, "entrypoint": false,
			})
		}
		updates = append(updates, map[string]any{
			"package_id": state.PackageID, "aggregate_sha256": state.AggregateSHA,
			"file_count": len(state.Files), "files": fileResults,
		})
	}
	return updates, nil
}

func (e *Executor) loadSmartPluginStates(instanceID string) ([]loadedSmartPluginState, error) {
	root, err := ensureDataSubdirectory(e.config.DataDir, "smart-plugins")
	if err != nil {
		return nil, err
	}
	instanceRoot, err := ResolveWithin(root, safeLogName(instanceID))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(instanceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > maxSmartPluginFiles {
		return nil, errors.New("instance contains too many smart plugin state entries")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	states := make([]loadedSmartPluginState, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || !info.IsDir() || isReparseOrSymlink(info) {
			continue
		}
		state, stateRoot, stateErr := e.readSmartPluginState(instanceID, entry.Name())
		if stateErr != nil {
			if e.logger != nil {
				e.logger.Printf("instance %s skip invalid smart plugin state %s while reconciling generated configs: %v", instanceID, entry.Name(), stateErr)
			}
			continue
		}
		states = append(states, loadedSmartPluginState{state: state, root: stateRoot})
	}
	return states, nil
}

func generatedConfigSMXBase(enabled bool, file smartPluginStateFile) (string, bool) {
	if !enabled || !file.Entrypoint || !strings.EqualFold(filepath.Ext(file.TargetPath), ".smx") {
		return "", false
	}
	lower := strings.ToLower(filepath.ToSlash(file.TargetPath))
	const prefix = "left4dead2/addons/sourcemod/plugins/"
	if !strings.HasPrefix(lower, prefix) || strings.HasPrefix(lower, prefix+"disabled/") {
		return "", false
	}
	base := filepath.Base(filepath.FromSlash(lower))
	return strings.TrimSuffix(base, ".smx"), true
}

func smartPluginAggregateSHA(files []smartPluginStateFile) string {
	aggregate := sha256.New()
	for _, file := range files {
		_, _ = io.WriteString(aggregate, file.TargetPath+"\x00"+file.SHA256+"\n")
	}
	return hex.EncodeToString(aggregate.Sum(nil))
}

func (e *Executor) attachSmartPluginGeneratedConfigs(ctx context.Context, instanceID, jobType string, registeredPaths map[string]string, waitForStartup bool, result map[string]any) map[string]any {
	// A nil map means the sending Control predates the server-derived ownership
	// snapshot. Keep compatibility fail-closed instead of guessing ownership.
	if registeredPaths == nil || jobType != "start" && jobType != "stop" && jobType != "restart" {
		return result
	}
	updates, err := e.reconcileSmartPluginGeneratedConfigs(instanceID, registeredPaths, "")
	if err != nil {
		if e.logger != nil {
			e.logger.Printf("instance %s reconcile generated SourceMod configs: %v", instanceID, err)
		}
		return result
	}
	if waitForStartup && (jobType == "start" || jobType == "restart") {
		later := e.pollSmartPluginGeneratedConfigs(ctx, instanceID, registeredPaths, smartPluginGeneratedConfigPollInterval, smartPluginGeneratedConfigMaxPollAttempts)
		updates = append(updates, later...)
	}
	if len(updates) > 0 {
		result["smart_plugin_config_updates"] = updates
	}
	return result
}

func (e *Executor) pollSmartPluginGeneratedConfigs(ctx context.Context, instanceID string, registeredPaths map[string]string, interval time.Duration, maxAttempts int) []map[string]any {
	if interval <= 0 || maxAttempts <= 0 {
		return nil
	}
	updates := make([]map[string]any, 0)
	for attempt := 0; attempt < maxAttempts && e.hasUntrackedGeneratedConfigCandidate(instanceID, registeredPaths); attempt++ {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return updates
		case <-timer.C:
		}
		later, err := e.reconcileSmartPluginGeneratedConfigs(instanceID, registeredPaths, "")
		if err != nil {
			if e.logger != nil {
				e.logger.Printf("instance %s recheck generated SourceMod configs after startup: %v", instanceID, err)
			}
			continue
		}
		updates = append(updates, later...)
	}
	return updates
}

func (e *Executor) hasUntrackedGeneratedConfigCandidate(instanceID string, registeredPaths map[string]string) bool {
	states, err := e.loadSmartPluginStates(instanceID)
	if err != nil {
		return false
	}
	ownedConfigs := make(map[string]bool)
	for path := range registeredPaths {
		ownedConfigs[strings.ToLower(filepath.ToSlash(strings.TrimSpace(path)))] = true
	}
	ownersByBase := make(map[string]map[int]bool)
	for index, loaded := range states {
		for _, file := range loaded.state.Files {
			if _, ok := generatedConfigManagedName(file.TargetPath); ok {
				ownedConfigs[strings.ToLower(file.TargetPath)] = true
			}
			base, ok := generatedConfigSMXBase(loaded.state.Enabled, file)
			if !ok {
				continue
			}
			if ownersByBase[base] == nil {
				ownersByBase[base] = make(map[int]bool)
			}
			ownersByBase[base][index] = true
		}
	}
	for base, owners := range ownersByBase {
		if len(owners) != 1 {
			continue
		}
		candidate := strings.ToLower(filepath.ToSlash(filepath.Join(smartPluginGeneratedConfigDirectory, base+".cfg")))
		if !ownedConfigs[candidate] {
			return true
		}
	}
	return false
}

func generatedConfigManagedName(path string) (string, bool) {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path))))
	if cleaned != path || !strings.EqualFold(filepath.Ext(cleaned), ".cfg") || !strings.EqualFold(filepath.ToSlash(filepath.Dir(filepath.FromSlash(cleaned))), smartPluginGeneratedConfigDirectory) {
		return "", false
	}
	return filepath.Base(filepath.FromSlash(cleaned)), true
}

func mergeSmartPluginConfigUpdates(target, source map[string]any) {
	sourceUpdates, ok := source["smart_plugin_config_updates"].([]map[string]any)
	if !ok || len(sourceUpdates) == 0 {
		return
	}
	targetUpdates, _ := target["smart_plugin_config_updates"].([]map[string]any)
	merged := make([]map[string]any, 0, len(sourceUpdates)+len(targetUpdates))
	merged = append(merged, sourceUpdates...)
	merged = append(merged, targetUpdates...)
	target["smart_plugin_config_updates"] = merged
}
