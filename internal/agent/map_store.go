package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxMapObjectSize = int64(2 << 30)

type mapRoot struct {
	Path         string
	FilesystemID string
}

type mapStore struct {
	config   *Config
	registry *mapRegistry
	roots    []mapRoot
	byFS     map[string]mapRoot
}

func newMapStore(config *Config) (*mapStore, error) {
	if config == nil {
		return nil, errors.New("Agent config is required")
	}
	if len(config.MapRoots) == 0 {
		config.MapRoots = []string{filepath.Join(config.DataDir, "maps")}
	}
	store := &mapStore{config: config, byFS: make(map[string]mapRoot)}
	for _, configured := range config.MapRoots {
		if err := os.MkdirAll(configured, 0o750); err != nil {
			return nil, fmt.Errorf("create map root %s: %w", configured, err)
		}
		if err := rejectLinkedAncestors(configured); err != nil {
			return nil, fmt.Errorf("map root %s is unsafe: %w", configured, err)
		}
		filesystemID, err := mapFilesystemID(configured)
		if err != nil {
			return nil, fmt.Errorf("identify map root filesystem %s: %w", configured, err)
		}
		if existing, duplicate := store.byFS[filesystemID]; duplicate {
			return nil, fmt.Errorf("map roots %s and %s are on the same filesystem", existing.Path, configured)
		}
		root := mapRoot{Path: filepath.Clean(configured), FilesystemID: filesystemID}
		store.roots = append(store.roots, root)
		store.byFS[filesystemID] = root
		for _, directory := range []string{"objects", "posters", "staging"} {
			if _, err := ensureDataSubdirectory(root.Path, directory); err != nil {
				return nil, fmt.Errorf("prepare map root %s: %w", root.Path, err)
			}
		}
	}
	registry, err := openMapRegistry(store.roots[0].Path)
	if err != nil {
		return nil, err
	}
	store.registry = registry
	if err := store.cleanupStaging(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *mapStore) defaultRoot() mapRoot { return s.roots[0] }

func (s *mapStore) findByAggregate(aggregateSHA256 string) (mapLocalRecord, bool) {
	for _, record := range s.registry.list() {
		if !strings.EqualFold(record.AggregateSHA256, aggregateSHA256) {
			continue
		}
		healthy := true
		for _, artifact := range record.Artifacts {
			artifactHealthy := false
			for filesystemID, relative := range artifact.Replicas {
				root, configured := s.byFS[filesystemID]
				if !configured {
					continue
				}
				sha, size, err := hashRegularFileWithinNoFollow(root.Path, relative, maxMapObjectSize)
				if err == nil && strings.EqualFold(sha, artifact.SHA256) && size == artifact.Size {
					artifactHealthy = true
					break
				}
			}
			if !artifactHealthy {
				healthy = false
				break
			}
		}
		if healthy {
			return record, true
		}
	}
	return mapLocalRecord{}, false
}

func (s *mapStore) cleanupStaging() error {
	staging, err := ResolveWithin(s.defaultRoot().Path, "staging")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := validateQQFlashEntryName(entry.Name()); err != nil {
			return errors.New("map staging contains an unsafe entry")
		}
		path, err := ResolveWithin(staging, entry.Name())
		if err != nil {
			return err
		}
		if err := removeMapOwnedTree(path); err != nil {
			return fmt.Errorf("clean stale map staging %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func removeMapOwnedTree(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if isReparseOrSymlink(info) {
		return ErrPathEscape
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("map staging contains a special file")
	}
	if info.IsDir() {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if isReparseOrSymlink(entryInfo) || (!entryInfo.IsDir() && !entryInfo.Mode().IsRegular()) {
				return ErrPathEscape
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return os.RemoveAll(root)
}

func mapObjectRelative(sha string) string {
	return filepath.ToSlash(filepath.Join("objects", sha[:2], sha+".vpk"))
}

func (s *mapStore) publish(record mapLocalRecord, outputs []string, posterOutputs ...string) (mapLocalRecord, error) {
	if len(outputs) != len(record.Artifacts) {
		return record, errors.New("map output count does not match artifact metadata")
	}
	root := s.defaultRoot()
	createdObjects := make([]string, 0, len(outputs))
	createdPosters := make([]string, 0, 1)
	rollback := func() {
		for _, relative := range append(createdObjects, createdPosters...) {
			if _, links, err := managedHardLinkIdentity(root.Path, relative); err == nil && links == 1 {
				_ = deleteManagedFileNoFollow(root.Path, relative)
			}
		}
	}
	for index, output := range outputs {
		artifact := &record.Artifacts[index]
		relative := mapObjectRelative(artifact.SHA256)
		existingSHA, existingSize, err := hashRegularFileWithinNoFollow(root.Path, relative, maxMapObjectSize)
		if errors.Is(err, os.ErrNotExist) {
			if err := copyRegularFileToRootNoFollow(output, root.Path, relative, secureWriteOptions{
				Mode: 0o440, ExpectedSHA256: artifact.SHA256, Replace: false, RejectExistingLink: true,
			}); err != nil {
				rollback()
				return record, fmt.Errorf("publish map object %d: %w", index, err)
			}
			createdObjects = append(createdObjects, relative)
		} else if err != nil {
			rollback()
			return record, fmt.Errorf("inspect existing map object %d: %w", index, err)
		} else if existingSHA != artifact.SHA256 || existingSize != artifact.Size {
			rollback()
			return record, errors.New("content-addressed map object does not match its digest")
		}
		file, err := openManagedFileNoFollow(root.Path, relative)
		if err != nil {
			rollback()
			return record, err
		}
		_ = file.Chmod(0o440)
		_ = file.Close()
		artifact.Replicas = map[string]string{root.FilesystemID: relative}
	}
	if len(posterOutputs) > 0 && posterOutputs[0] != "" {
		if record.Poster.MIME != "image/jpeg" || !validUploadSHA256(record.Poster.SHA256) || record.Poster.Size <= 0 || record.Poster.Size > maxMapPosterJPEG {
			rollback()
			return record, errors.New("map poster metadata is invalid")
		}
		relative := filepath.ToSlash(filepath.Join("posters", record.ID+".jpg"))
		existingSHA, existingSize, err := hashRegularFileWithinNoFollow(root.Path, relative, maxMapPosterJPEG)
		if errors.Is(err, os.ErrNotExist) {
			if err := copyRegularFileToRootNoFollow(posterOutputs[0], root.Path, relative, secureWriteOptions{
				Mode: 0o440, ExpectedSHA256: record.Poster.SHA256, Replace: false, RejectExistingLink: true,
			}); err != nil {
				rollback()
				return record, fmt.Errorf("publish map poster: %w", err)
			}
			createdPosters = append(createdPosters, relative)
		} else if err != nil || existingSHA != record.Poster.SHA256 || existingSize != record.Poster.Size {
			rollback()
			return record, errors.New("published map poster is inconsistent")
		}
		record.Poster.Relative = relative
	}
	if record.Assignments == nil {
		record.Assignments = make(map[string]mapLocalAssignment)
	}
	canonical, duplicate, err := s.registry.putIfAggregateAbsent(record)
	if err != nil {
		rollback()
		return record, err
	}
	if duplicate {
		// Content-addressed objects may already be referenced by the canonical
		// record, so keep them. Only the duplicate's map-ID-specific poster is
		// safe to remove.
		for _, relative := range createdPosters {
			if err := deleteManagedFileNoFollow(root.Path, relative); err != nil && !errors.Is(err, os.ErrNotExist) {
				return record, fmt.Errorf("remove duplicate map poster: %w", err)
			}
		}
		return canonical, nil
	}
	return canonical, nil
}

func (s *mapStore) ensureReplica(record *mapLocalRecord, artifactIndex int, destination mapRoot) error {
	artifact := &record.Artifacts[artifactIndex]
	if relative := artifact.Replicas[destination.FilesystemID]; relative != "" {
		sha, size, err := hashRegularFileWithinNoFollow(destination.Path, relative, maxMapObjectSize)
		if err == nil && sha == artifact.SHA256 && size == artifact.Size {
			return nil
		}
		if err == nil {
			_, links, identityErr := managedHardLinkIdentity(destination.Path, relative)
			if identityErr != nil || links > 1 {
				return errors.New("corrupt map replica has active or unknown hard links")
			}
			if err := deleteManagedFileNoFollow(destination.Path, relative); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			_, links, identityErr := managedHardLinkIdentity(destination.Path, relative)
			if identityErr != nil || links > 1 {
				return errors.New("corrupt map replica cannot be safely replaced")
			}
			if deleteErr := deleteManagedFileNoFollow(destination.Path, relative); deleteErr != nil {
				return deleteErr
			}
		}
	}
	var sourceRoot mapRoot
	var sourceRelative string
	for filesystemID, relative := range artifact.Replicas {
		root, ok := s.byFS[filesystemID]
		if !ok {
			continue
		}
		sha, size, err := hashRegularFileWithinNoFollow(root.Path, relative, maxMapObjectSize)
		if err == nil && sha == artifact.SHA256 && size == artifact.Size {
			sourceRoot, sourceRelative = root, relative
			break
		}
	}
	if sourceRelative == "" {
		return errors.New("map has no healthy replica for this artifact")
	}
	destinationRelative := mapObjectRelative(artifact.SHA256)
	sourcePath, err := ResolveWithin(sourceRoot.Path, filepath.FromSlash(sourceRelative))
	if err != nil {
		return err
	}
	if err := copyRegularFileToRootNoFollow(sourcePath, destination.Path, destinationRelative, secureWriteOptions{
		Mode: 0o440, ExpectedSHA256: artifact.SHA256, Replace: false, RejectExistingLink: true,
	}); err != nil {
		if existingSHA, existingSize, hashErr := hashRegularFileWithinNoFollow(destination.Path, destinationRelative, maxMapObjectSize); hashErr != nil || existingSHA != artifact.SHA256 || existingSize != artifact.Size {
			return fmt.Errorf("replicate map object: %w", err)
		}
	}
	artifact.Replicas[destination.FilesystemID] = destinationRelative
	return nil
}

func (s *mapStore) assign(mapID string, instance InstanceRecord, instancePath string) error {
	record, ok := s.registry.get(mapID)
	if !ok {
		return errors.New("map does not exist in the node library")
	}
	filesystemID, err := mapFilesystemID(instancePath)
	if err != nil {
		return err
	}
	root, ok := s.byFS[filesystemID]
	if !ok {
		return errors.New("no configured map root is on the instance filesystem")
	}
	if existing, exists := record.Assignments[instance.ID]; exists {
		if existing.FilesystemID != filesystemID || len(existing.Targets) != len(record.Artifacts) {
			return errors.New("existing map assignment metadata is inconsistent")
		}
		for index, target := range existing.Targets {
			sourceRelative := record.Artifacts[index].Replicas[filesystemID]
			sourceID, _, sourceErr := managedHardLinkIdentity(root.Path, sourceRelative)
			targetID, _, targetErr := managedHardLinkIdentity(instancePath, target)
			targetSHA, targetSize, hashErr := hashRegularFileWithinNoFollow(instancePath, target, maxMapObjectSize)
			if sourceErr != nil || targetErr != nil || hashErr != nil || sourceID != targetID ||
				targetSHA != record.Artifacts[index].SHA256 || targetSize != record.Artifacts[index].Size {
				return errors.New("existing map assignment link is missing or inconsistent")
			}
		}
		return nil
	}

	for index := range record.Artifacts {
		if err := s.ensureReplica(&record, index, root); err != nil {
			return err
		}
	}
	targets := make([]string, len(record.Artifacts))
	created := make([]string, 0, len(record.Artifacts))
	type replacedStandaloneTarget struct {
		target         string
		sourceRelative string
		sha256         string
		size           int64
	}
	replacedStandalone := make([]replacedStandaloneTarget, 0)
	rollback := func() error {
		var rollbackErr error
		for _, createdTarget := range created {
			if err := deleteManagedFileNoFollow(instancePath, createdTarget); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
		for _, replaced := range replacedStandalone {
			sourcePath, err := ResolveWithin(root.Path, filepath.FromSlash(replaced.sourceRelative))
			if err == nil {
				err = copyRegularFileToRootNoFollow(sourcePath, instancePath, replaced.target, secureWriteOptions{
					Mode: 0o440, ExpectedSize: replaced.size, ExpectedSHA256: replaced.sha256, Replace: false, RejectExistingLink: true,
				})
			}
			rollbackErr = errors.Join(rollbackErr, err)
		}
		return rollbackErr
	}
	for index, artifact := range record.Artifacts {
		target := mapTargetRelative(mapID, index)
		targets[index] = target
		sourceRelative := artifact.Replicas[filesystemID]
		sourceID, _, sourceErr := managedHardLinkIdentity(root.Path, sourceRelative)
		if sourceErr != nil {
			return errors.Join(sourceErr, rollback())
		}
		targetID, _, beforeErr := managedHardLinkIdentity(instancePath, target)
		if beforeErr == nil && targetID != sourceID {
			targetSHA, targetSize, hashErr := hashRegularFileWithinNoFollow(instancePath, target, maxMapObjectSize)
			if hashErr != nil || targetSHA != artifact.SHA256 || targetSize != artifact.Size {
				return errors.Join(errors.New("map assignment target already exists with different content"), rollback())
			}
			if err := deleteManagedFileNoFollow(instancePath, target); err != nil {
				return errors.Join(err, rollback())
			}
			replacedStandalone = append(replacedStandalone, replacedStandaloneTarget{
				target: target, sourceRelative: sourceRelative, sha256: artifact.SHA256, size: artifact.Size,
			})
			beforeErr = os.ErrNotExist
		} else if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
			return errors.Join(beforeErr, rollback())
		}
		if err := createManagedHardLink(root.Path, artifact.Replicas[filesystemID], instancePath, target); err != nil {
			return errors.Join(err, rollback())
		}
		if errors.Is(beforeErr, os.ErrNotExist) {
			created = append(created, target)
		}
		sourceID, _, sourceErr = managedHardLinkIdentity(root.Path, artifact.Replicas[filesystemID])
		targetID, _, targetErr := managedHardLinkIdentity(instancePath, target)
		targetSHA, targetSize, hashErr := hashRegularFileWithinNoFollow(instancePath, target, maxMapObjectSize)
		sourceFilesystemID, sourceFSErr := mapFilesystemID(root.Path)
		targetFilesystemID, targetFSErr := mapFilesystemID(instancePath)
		if sourceErr != nil || targetErr != nil || hashErr != nil || sourceFSErr != nil || targetFSErr != nil ||
			sourceID != targetID || sourceFilesystemID != filesystemID || targetFilesystemID != filesystemID ||
			targetSHA != artifact.SHA256 || targetSize != artifact.Size {
			return errors.Join(errors.New("map hard link failed post-creation identity or checksum verification"), rollback())
		}
	}
	record.Assignments[instance.ID] = mapLocalAssignment{InstanceID: instance.ID, FilesystemID: filesystemID, Targets: targets}
	if err := s.registry.put(record); err != nil {
		return errors.Join(err, rollback())
	}
	return nil
}

func (s *mapStore) unassign(mapID string, instance InstanceRecord, instancePath string) error {
	record, ok := s.registry.get(mapID)
	if !ok {
		return errors.New("map does not exist in the node library")
	}
	assignment, exists := record.Assignments[instance.ID]
	if !exists {
		return nil
	}
	root, ok := s.byFS[assignment.FilesystemID]
	if !ok {
		return errors.New("map assignment references an unconfigured filesystem")
	}
	for index, target := range assignment.Targets {
		sourceRelative := record.Artifacts[index].Replicas[assignment.FilesystemID]
		sourceID, _, err := managedHardLinkIdentity(root.Path, sourceRelative)
		if err != nil {
			return err
		}
		targetID, _, err := managedHardLinkIdentity(instancePath, target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || sourceID != targetID {
			return errors.New("refusing to remove an unknown map assignment file")
		}
		if err := deleteManagedFileNoFollow(instancePath, target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	delete(record.Assignments, instance.ID)
	return s.registry.put(record)
}

func (s *mapStore) verify(mapID string, instances *Registry, paths *PathGuard) (string, []map[string]any, error) {
	record, ok := s.registry.get(mapID)
	if !ok {
		return "", nil, errors.New("map does not exist in the node library")
	}
	rootHealth := make(map[string]string)
	brokenArtifacts := 0
	degraded := false
	repaired := false
	for artifactIndex := range record.Artifacts {
		artifact := &record.Artifacts[artifactIndex]
		for filesystemID := range artifact.Replicas {
			root, configured := s.byFS[filesystemID]
			if !configured {
				continue
			}
			sha, size, hashErr := hashRegularFileWithinNoFollow(root.Path, artifact.Replicas[filesystemID], maxMapObjectSize)
			if hashErr == nil && sha == artifact.SHA256 && size == artifact.Size {
				continue
			}
			if repairErr := s.ensureReplica(&record, artifactIndex, root); repairErr == nil {
				repaired = true
			}
		}
		healthy := 0
		for filesystemID, relative := range artifact.Replicas {
			root, configured := s.byFS[filesystemID]
			if !configured {
				setMapReplicaHealth(rootHealth, filesystemID, "missing")
				degraded = true
				continue
			}
			sha, size, err := hashRegularFileWithinNoFollow(root.Path, relative, maxMapObjectSize)
			if err == nil && sha == artifact.SHA256 && size == artifact.Size {
				setMapReplicaHealth(rootHealth, filesystemID, "healthy")
				healthy++
			} else {
				setMapReplicaHealth(rootHealth, filesystemID, "corrupt")
				degraded = true
			}
		}
		if healthy == 0 {
			brokenArtifacts++
		}
	}
	for _, assignment := range record.Assignments {
		root, ok := s.byFS[assignment.FilesystemID]
		instance, instanceOK := findInstanceRecordByID(assignment.InstanceID, instances)
		if !ok || !instanceOK {
			degraded = true
			continue
		}
		instancePath, err := resolveInstanceRecordPath(paths, instance)
		if err != nil || len(assignment.Targets) != len(record.Artifacts) {
			degraded = true
			continue
		}
		for index, target := range assignment.Targets {
			sourceRelative := record.Artifacts[index].Replicas[assignment.FilesystemID]
			sourceID, _, sourceErr := managedHardLinkIdentity(root.Path, sourceRelative)
			targetID, _, targetErr := managedHardLinkIdentity(instancePath, target)
			if sourceErr != nil || targetErr != nil || sourceID != targetID {
				degraded = true
			}
		}
	}
	if repaired {
		if err := s.registry.put(record); err != nil {
			return "", nil, err
		}
	}
	health := "healthy"
	if brokenArtifacts > 0 {
		health = "broken"
	} else if degraded {
		health = "degraded"
	}
	replicas := make([]map[string]any, 0, len(rootHealth))
	for filesystemID, replicaHealth := range rootHealth {
		replicas = append(replicas, map[string]any{"filesystem_id": filesystemID, "health": replicaHealth})
	}
	sort.Slice(replicas, func(i, j int) bool {
		return mapResultString(replicas[i], "filesystem_id") < mapResultString(replicas[j], "filesystem_id")
	})
	return health, replicas, nil
}

func setMapReplicaHealth(values map[string]string, filesystemID, health string) {
	severity := map[string]int{"healthy": 0, "missing": 1, "corrupt": 2}
	if current, exists := values[filesystemID]; !exists || severity[health] > severity[current] {
		values[filesystemID] = health
	}
}

// findInstanceRecordByID is replaced by Executor-level verification where the
// live instance registry is available. A nil registry deliberately reports no
// record and keeps standalone map-store verification fail closed.
func findInstanceRecordByID(id string, registry *Registry) (InstanceRecord, bool) {
	if registry == nil {
		return InstanceRecord{}, false
	}
	return registry.Get(id)
}

func (s *mapStore) delete(mapID string) error {
	record, ok := s.registry.get(mapID)
	if !ok {
		return nil
	}
	if len(record.Assignments) != 0 {
		return errors.New("map is still assigned to one or more instances")
	}
	allRecords := s.registry.list()
	posterPresent := false
	if record.Poster.Relative != "" {
		root := s.defaultRoot()
		sha, size, err := hashRegularFileWithinNoFollow(root.Path, record.Poster.Relative, maxMapPosterJPEG)
		if err == nil {
			if sha != record.Poster.SHA256 || size != record.Poster.Size {
				return errors.New("map poster checksum is inconsistent")
			}
			posterPresent = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, artifact := range record.Artifacts {
		for filesystemID, relative := range artifact.Replicas {
			if mapObjectReferencedByOtherRecord(allRecords, mapID, filesystemID, relative) {
				continue
			}
			root, ok := s.byFS[filesystemID]
			if !ok {
				return errors.New("map replica is on an unconfigured filesystem")
			}
			sha, size, hashErr := hashRegularFileWithinNoFollow(root.Path, relative, maxMapObjectSize)
			if errors.Is(hashErr, os.ErrNotExist) {
				continue
			}
			if hashErr != nil || sha != artifact.SHA256 || size != artifact.Size {
				return errors.New("map replica checksum is inconsistent; verify the map before retrying deletion")
			}
			_, links, err := managedHardLinkIdentity(root.Path, relative)
			if err != nil {
				return err
			}
			if links != 1 {
				return fmt.Errorf("map object has %d links; expected exactly one", links)
			}
			if err := deleteManagedFileNoFollow(root.Path, relative); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if posterPresent {
		root := s.defaultRoot()
		if err := deleteManagedFileNoFollow(root.Path, record.Poster.Relative); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.registry.delete(mapID)
}

func mapObjectReferencedByOtherRecord(records []mapLocalRecord, excludedMapID, filesystemID, relative string) bool {
	for _, record := range records {
		if record.ID == excludedMapID {
			continue
		}
		for _, artifact := range record.Artifacts {
			if strings.EqualFold(artifact.Replicas[filesystemID], relative) {
				return true
			}
		}
	}
	return false
}

func (s *mapStore) importResult(record mapLocalRecord) map[string]any {
	artifacts := make([]map[string]any, len(record.Artifacts))
	replicaSet := make(map[string]bool)
	for index, artifact := range record.Artifacts {
		artifacts[index] = map[string]any{"ordinal": artifact.Ordinal, "sha256": artifact.SHA256, "size": artifact.Size}
		for filesystemID := range artifact.Replicas {
			replicaSet[filesystemID] = true
		}
	}
	bsps := make([]map[string]any, len(record.BSPs))
	for index, bsp := range record.BSPs {
		bsps[index] = map[string]any{"virtual_path": bsp.VirtualPath, "start_name": bsp.StartName, "dictionary_present": bsp.DictionaryPresent}
	}
	modes := make([]map[string]any, len(record.Modes))
	for modeIndex, mode := range record.Modes {
		chapters := make([]map[string]any, len(mode.Chapters))
		for chapterIndex, chapter := range mode.Chapters {
			chapters[chapterIndex] = map[string]any{"ordinal": chapter.Ordinal, "bsp_name": chapter.BSPName,
				"display_name": chapter.DisplayName, "image": chapter.Image}
		}
		modes[modeIndex] = map[string]any{"name": mode.Name, "chapters": chapters}
	}
	warnings := make([]map[string]any, len(record.Warnings))
	for index, warning := range record.Warnings {
		warnings[index] = map[string]any{"code": warning.Code, "message": warning.Message, "virtual_path": warning.VirtualPath}
	}
	replicas := make([]map[string]any, 0, len(replicaSet))
	for filesystemID := range replicaSet {
		replicas = append(replicas, map[string]any{"filesystem_id": filesystemID, "health": "healthy"})
	}
	sort.Slice(replicas, func(i, j int) bool {
		return mapResultString(replicas[i], "filesystem_id") < mapResultString(replicas[j], "filesystem_id")
	})
	result := map[string]any{"map_id": record.ID, "name": record.Name, "author": record.Author, "version": record.Version,
		"description": record.Description, "aggregate_sha256": record.AggregateSHA256,
		"source_name": record.SourceName, "total_size": record.TotalSize, "removed_files": record.RemovedFiles,
		"removed_bytes": record.RemovedBytes, "artifacts": artifacts, "bsps": bsps, "modes": modes, "warnings": warnings, "replicas": replicas}
	if record.Poster.Relative != "" {
		if raw, err := readRegularFileWithinNoFollow(s.defaultRoot().Path, record.Poster.Relative, maxMapPosterJPEG); err == nil && int64(len(raw)) == record.Poster.Size {
			sum := sha256.Sum256(raw)
			if fmt.Sprintf("%x", sum[:]) == record.Poster.SHA256 {
				result["poster_mime"] = record.Poster.MIME
				result["poster_base64"] = base64.StdEncoding.EncodeToString(raw)
			}
		}
	}
	return result
}

func (s *mapStore) backupReferences(instanceID, instancePath string) ([]BackupMapReference, error) {
	references := make([]BackupMapReference, 0)
	for _, record := range s.registry.list() {
		assignment, assigned := record.Assignments[instanceID]
		if !assigned {
			continue
		}
		if len(assignment.Targets) != len(record.Artifacts) {
			return nil, errors.New("map assignment metadata is inconsistent")
		}
		root, ok := s.byFS[assignment.FilesystemID]
		if !ok {
			return nil, errors.New("map assignment references an unconfigured filesystem")
		}
		for index, target := range assignment.Targets {
			sourceID, _, sourceErr := managedHardLinkIdentity(root.Path, record.Artifacts[index].Replicas[assignment.FilesystemID])
			targetID, _, targetErr := managedHardLinkIdentity(instancePath, target)
			if sourceErr != nil || targetErr != nil || sourceID != targetID {
				return nil, errors.New("map assignment link failed verification before backup")
			}
		}
		bsps := make([]string, len(record.BSPs))
		for index, bsp := range record.BSPs {
			bsps[index] = bsp.VirtualPath
		}
		sort.Strings(bsps)
		references = append(references, BackupMapReference{ID: record.ID, AggregateSHA256: record.AggregateSHA256, Name: record.Name, BSPs: bsps})
	}
	sort.Slice(references, func(i, j int) bool { return references[i].ID < references[j].ID })
	return references, nil
}

func (s *mapStore) findForBackupReference(reference BackupMapReference) (mapLocalRecord, bool) {
	if record, ok := s.registry.get(reference.ID); ok {
		return record, true
	}
	for _, record := range s.registry.list() {
		if strings.EqualFold(record.AggregateSHA256, reference.AggregateSHA256) {
			return record, true
		}
	}
	return mapLocalRecord{}, false
}

func (s *mapStore) restoreBackupAssignments(instanceID string, instance InstanceRecord, instancePath string, references []BackupMapReference) ([]BackupMapReference, error) {
	desired := make(map[string]mapLocalRecord)
	missing := make([]BackupMapReference, 0)
	for _, reference := range references {
		record, ok := s.findForBackupReference(reference)
		if !ok {
			missing = append(missing, reference)
			continue
		}
		desired[record.ID] = record
	}
	for _, record := range s.registry.list() {
		if _, assigned := record.Assignments[instanceID]; !assigned {
			continue
		}
		if _, keep := desired[record.ID]; keep {
			continue
		}
		if err := s.unassign(record.ID, instance, instancePath); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.assign(id, instance, instancePath); err != nil {
			return nil, err
		}
	}
	return missing, nil
}

func backupMapContainsStartName(reference BackupMapReference, startName string) bool {
	for _, virtualPath := range reference.BSPs {
		candidate := strings.TrimSuffix(filepath.Base(filepath.FromSlash(virtualPath)), filepath.Ext(filepath.FromSlash(virtualPath)))
		if strings.EqualFold(candidate, startName) {
			return true
		}
	}
	return false
}

func mapResultString(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func mapAssignmentResult(mapID, instanceID string) map[string]any {
	return map[string]any{"map_id": mapID, "instance_id": instanceID, "restart_required": false}
}

func mapProcessRunning(state ProcessState) bool {
	return state.Status == "running" || state.Status == "degraded" || state.Status == "external"
}
