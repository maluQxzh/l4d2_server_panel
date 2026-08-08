package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	mapRegistryVersion = 1
	mapAddonDirectory  = "left4dead2/addons/workshop"
)

type mapLocalArtifact struct {
	Ordinal  int               `json:"ordinal"`
	SHA256   string            `json:"sha256"`
	Size     int64             `json:"size"`
	Replicas map[string]string `json:"replicas"`
}

type mapLocalBSP struct {
	VirtualPath       string `json:"virtual_path"`
	StartName         string `json:"start_name,omitempty"`
	DictionaryPresent bool   `json:"dictionary_present"`
}

type mapLocalWarning struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	VirtualPath string `json:"virtual_path,omitempty"`
}

type mapLocalAssignment struct {
	InstanceID   string   `json:"instance_id"`
	FilesystemID string   `json:"filesystem_id"`
	Targets      []string `json:"targets"`
}

type mapLocalRecord struct {
	ID              string                        `json:"id"`
	Name            string                        `json:"name"`
	Author          string                        `json:"author,omitempty"`
	Version         string                        `json:"version,omitempty"`
	Description     string                        `json:"description,omitempty"`
	AggregateSHA256 string                        `json:"aggregate_sha256"`
	SourceName      string                        `json:"source_name,omitempty"`
	TotalSize       int64                         `json:"total_size"`
	RemovedFiles    int                           `json:"removed_files"`
	RemovedBytes    int64                         `json:"removed_bytes"`
	Artifacts       []mapLocalArtifact            `json:"artifacts"`
	BSPs            []mapLocalBSP                 `json:"bsps"`
	Modes           []mapLocalMode                `json:"modes,omitempty"`
	Poster          mapLocalPoster                `json:"poster,omitempty"`
	Warnings        []mapLocalWarning             `json:"warnings,omitempty"`
	Assignments     map[string]mapLocalAssignment `json:"assignments,omitempty"`
}

type mapRegistryDocument struct {
	Version int                       `json:"version"`
	Maps    map[string]mapLocalRecord `json:"maps"`
}

type mapRegistry struct {
	mu   sync.RWMutex
	root string
	doc  mapRegistryDocument
}

func openMapRegistry(defaultMapRoot string) (*mapRegistry, error) {
	registry := &mapRegistry{root: defaultMapRoot, doc: mapRegistryDocument{Version: mapRegistryVersion, Maps: make(map[string]mapLocalRecord)}}
	raw, err := readRegularFileWithinNoFollow(defaultMapRoot, "registry.json", 32<<20)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read map registry: %w", err)
	}
	if err := json.Unmarshal(raw, &registry.doc); err != nil {
		return nil, fmt.Errorf("decode map registry: %w", err)
	}
	if registry.doc.Version != mapRegistryVersion || registry.doc.Maps == nil || len(registry.doc.Maps) > 10000 {
		return nil, errors.New("map registry version or size is invalid")
	}
	for id, record := range registry.doc.Maps {
		if id != record.ID {
			return nil, fmt.Errorf("invalid map registry record %q: identity mismatch", id)
		}
		if err := validateLocalMapRecord(record); err != nil {
			return nil, fmt.Errorf("invalid map registry record %q: %w", id, err)
		}
	}
	return registry, nil
}

func validateLocalMapRecord(record mapLocalRecord) error {
	if uuid.Validate(record.ID) != nil || record.Name == "" || utf8.RuneCountInString(record.Name) > 255 || !utf8.ValidString(record.Name) || strings.ContainsAny(record.Name, "\x00\r\n") ||
		!validUploadSHA256(record.AggregateSHA256) || record.TotalSize <= 0 || len(record.Artifacts) == 0 || len(record.Artifacts) > 64 {
		return errors.New("invalid map identity or artifact count")
	}
	if !validMapMetadataText(record.Author, 255) || !validMapMetadataText(record.Version, 64) || !validMapMetadataText(record.Description, 2048) {
		return errors.New("invalid map mission metadata")
	}
	for index, artifact := range record.Artifacts {
		if artifact.Ordinal != index || !validUploadSHA256(artifact.SHA256) || artifact.Size <= 0 || len(artifact.Replicas) == 0 || len(artifact.Replicas) > 64 {
			return errors.New("invalid map artifact")
		}
		for filesystemID, relative := range artifact.Replicas {
			if filesystemID == "" || !validMapObjectRelative(relative, artifact.SHA256) {
				return errors.New("invalid map replica")
			}
		}
	}
	if len(record.BSPs) == 0 || len(record.BSPs) > 4096 || len(record.Assignments) > 10000 {
		return errors.New("invalid map BSP or assignment count")
	}
	chapterCount := 0
	seenModes := make(map[string]bool)
	for _, mode := range record.Modes {
		if !validSourceMapName(mode.Name) || len(mode.Name) > 64 || seenModes[strings.ToLower(mode.Name)] || len(mode.Chapters) == 0 || len(mode.Chapters) > 4096 {
			return errors.New("invalid map mission mode")
		}
		seenModes[strings.ToLower(mode.Name)] = true
		seenOrdinals := make(map[int]bool)
		for _, chapter := range mode.Chapters {
			if chapter.Ordinal < 1 || chapter.Ordinal > 4096 || seenOrdinals[chapter.Ordinal] || !validSourceMapName(chapter.BSPName) ||
				!validMapMetadataText(chapter.DisplayName, 255) || chapter.DisplayName == "" || !validMapMetadataText(chapter.Image, 512) {
				return errors.New("invalid map mission chapter")
			}
			seenOrdinals[chapter.Ordinal] = true
			chapterCount++
		}
	}
	if len(record.Modes) > 64 || chapterCount > 4096 {
		return errors.New("map mission metadata exceeds limits")
	}
	if record.Poster.Relative != "" || record.Poster.SHA256 != "" || record.Poster.Size != 0 || record.Poster.MIME != "" {
		if record.Poster.MIME != "image/jpeg" || !validUploadSHA256(record.Poster.SHA256) || record.Poster.Size <= 0 || record.Poster.Size > maxMapPosterJPEG ||
			!validMapPosterRelative(record.ID, record.Poster.Relative) {
			return errors.New("invalid map poster")
		}
	}
	for _, warning := range record.Warnings {
		if warning.Code == "" || len(warning.Code) > 64 || warning.Message == "" || len(warning.Message) > 1024 || len(warning.VirtualPath) > 512 {
			return errors.New("invalid map warning")
		}
	}
	for instanceID, assignment := range record.Assignments {
		if instanceID == "" || assignment.InstanceID != instanceID || assignment.FilesystemID == "" || len(assignment.Targets) != len(record.Artifacts) {
			return errors.New("invalid map assignment")
		}
		for _, target := range assignment.Targets {
			if !validMapTargetRelative(record.ID, target) {
				return errors.New("invalid map assignment target")
			}
		}
	}
	return nil
}

func validMapMetadataText(value string, limit int) bool {
	return utf8.RuneCountInString(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validMapPosterRelative(mapID, relative string) bool {
	want := filepath.ToSlash(filepath.Join("posters", mapID+".jpg"))
	return relative == want
}

func validMapObjectRelative(relative, sha string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if cleaned != relative || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") || strings.Contains(relative, "\\") {
		return false
	}
	for _, component := range strings.Split(cleaned, "/") {
		if validateQQFlashEntryName(component) != nil {
			return false
		}
	}
	want := filepath.ToSlash(filepath.Join("objects", sha[:2], sha+".vpk"))
	return strings.EqualFold(cleaned, want)
}

func validMapTargetRelative(mapID, relative string) bool {
	cleaned, err := cleanManagedRelative(filepath.ToSlash(relative))
	if err != nil || cleaned != filepath.ToSlash(relative) {
		return false
	}
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(cleaned)))
	if dir != mapAddonDirectory {
		return false
	}
	base := filepath.Base(filepath.FromSlash(cleaned))
	return strings.HasPrefix(strings.ToLower(base), "l4d2panel-map-"+strings.ToLower(mapID)+"-") && strings.HasSuffix(strings.ToLower(base), ".vpk")
}

func mapTargetRelative(mapID string, index int) string {
	return filepath.ToSlash(filepath.Join(mapAddonDirectory, fmt.Sprintf("l4d2panel-map-%s-%02d.vpk", mapID, index)))
}

func isReservedMapAddonBase(base string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(filepath.FromSlash(base))), "l4d2panel-map-")
}

func isReservedMapAddonPath(relative string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(cleaned)))
	return dir == mapAddonDirectory && isReservedMapAddonBase(filepath.Base(filepath.FromSlash(cleaned)))
}

func (r *mapRegistry) get(id string) (mapLocalRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.doc.Maps[id]
	return cloneLocalMapRecord(record), ok
}

func (r *mapRegistry) list() []mapLocalRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]mapLocalRecord, 0, len(r.doc.Maps))
	for _, record := range r.doc.Maps {
		result = append(result, cloneLocalMapRecord(record))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *mapRegistry) put(record mapLocalRecord) error {
	if err := validateLocalMapRecord(record); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, existed := r.doc.Maps[record.ID]
	r.doc.Maps[record.ID] = cloneLocalMapRecord(record)
	if err := r.persistLocked(); err != nil {
		if existed {
			r.doc.Maps[record.ID] = old
		} else {
			delete(r.doc.Maps, record.ID)
		}
		return err
	}
	return nil
}

func (r *mapRegistry) delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, exists := r.doc.Maps[id]
	if !exists {
		return nil
	}
	delete(r.doc.Maps, id)
	if err := r.persistLocked(); err != nil {
		r.doc.Maps[id] = old
		return err
	}
	return nil
}

func (r *mapRegistry) persistLocked() error {
	raw, err := json.MarshalIndent(r.doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeRawFileWithinNoFollow(r.root, "registry.json", raw, 0o600, true)
}

func cloneLocalMapRecord(record mapLocalRecord) mapLocalRecord {
	cloned := record
	cloned.Artifacts = make([]mapLocalArtifact, len(record.Artifacts))
	for index, artifact := range record.Artifacts {
		cloned.Artifacts[index] = artifact
		cloned.Artifacts[index].Replicas = make(map[string]string, len(artifact.Replicas))
		for key, value := range artifact.Replicas {
			cloned.Artifacts[index].Replicas[key] = value
		}
	}
	cloned.BSPs = append([]mapLocalBSP(nil), record.BSPs...)
	cloned.Modes = make([]mapLocalMode, len(record.Modes))
	for index, mode := range record.Modes {
		cloned.Modes[index] = mode
		cloned.Modes[index].Chapters = append([]mapLocalChapter(nil), mode.Chapters...)
	}
	cloned.Warnings = append([]mapLocalWarning(nil), record.Warnings...)
	cloned.Assignments = make(map[string]mapLocalAssignment, len(record.Assignments))
	for key, assignment := range record.Assignments {
		assignment.Targets = append([]string(nil), assignment.Targets...)
		cloned.Assignments[key] = assignment
	}
	return cloned
}
