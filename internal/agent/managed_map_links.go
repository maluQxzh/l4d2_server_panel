package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type managedMapKnownPath struct {
	identity string
	links    uint64
}

// ManagedMapHardLinks returns the complete, registry-proven set of paths that
// participate in map-library hard links. It fails closed when a link count
// proves that an additional, unknown path references one of the objects.
func ManagedMapHardLinks(config *Config) (map[string]bool, error) {
	store, err := newMapStore(config)
	if err != nil {
		return nil, err
	}
	instances, err := OpenRegistry(config.DataDir)
	if err != nil {
		return nil, err
	}
	paths, err := NewPathGuard(config.InstanceRoots)
	if err != nil {
		return nil, err
	}
	known := make(map[string]managedMapKnownPath)
	for _, record := range store.registry.list() {
		for _, artifact := range record.Artifacts {
			for filesystemID, relative := range artifact.Replicas {
				root, ok := store.byFS[filesystemID]
				if !ok {
					return nil, errors.New("map registry references an unconfigured filesystem")
				}
				if err := addKnownManagedMapPath(known, root.Path, relative); err != nil {
					return nil, err
				}
			}
		}
		for _, assignment := range record.Assignments {
			instance, ok := instances.Get(assignment.InstanceID)
			if !ok {
				return nil, fmt.Errorf("map assignment references unknown instance %s", assignment.InstanceID)
			}
			instancePath, err := resolveInstanceRecordPath(paths, instance)
			if err != nil {
				return nil, err
			}
			if len(assignment.Targets) != len(record.Artifacts) {
				return nil, errors.New("map assignment target count is inconsistent")
			}
			for index, target := range assignment.Targets {
				if err := addKnownManagedMapPath(known, instancePath, target); err != nil {
					return nil, err
				}
				sourceRoot, ok := store.byFS[assignment.FilesystemID]
				if !ok {
					return nil, errors.New("map assignment references an unconfigured filesystem")
				}
				sourceRelative := record.Artifacts[index].Replicas[assignment.FilesystemID]
				sourceID, _, err := managedHardLinkIdentity(sourceRoot.Path, sourceRelative)
				if err != nil || sourceID != known[managedMapAbsoluteKey(instancePath, target)].identity {
					return nil, errors.New("map assignment does not reference its registered object")
				}
			}
		}
	}
	counts := make(map[string]uint64)
	actualLinks := make(map[string]uint64)
	for _, item := range known {
		counts[item.identity]++
		actualLinks[item.identity] = item.links
	}
	allowed := make(map[string]bool)
	for key, item := range known {
		if item.links > 1 {
			if counts[item.identity] != actualLinks[item.identity] {
				return nil, fmt.Errorf("map object has %d registered paths but filesystem link count is %d", counts[item.identity], actualLinks[item.identity])
			}
			allowed[key] = true
		}
	}
	return allowed, nil
}

func addKnownManagedMapPath(known map[string]managedMapKnownPath, root, relative string) error {
	key := managedMapAbsoluteKey(root, relative)
	if _, exists := known[key]; exists {
		return nil
	}
	identity, links, err := managedHardLinkIdentity(root, relative)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("registered map hard-link path is missing")
		}
		return err
	}
	if _, err := ResolveWithin(root, filepath.FromSlash(relative)); err != nil {
		return err
	}
	known[key] = managedMapKnownPath{identity: identity, links: links}
	return nil
}

func managedMapAbsoluteKey(root, relative string) string {
	absolute := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	return absolute
}
