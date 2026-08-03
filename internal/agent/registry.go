package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var ErrInstancePathRegistered = errors.New("instance path is already registered")

type InstanceRecord struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	RootIndex    int      `json:"root_index"`
	RelativePath string   `json:"relative_path"`
	Executable   string   `json:"executable,omitempty"`
	Args         []string `json:"args,omitempty"`
	AutoRestart  bool     `json:"auto_restart"`
	// DesiredRunning records the operator's durable intent independently from
	// the crash-restart policy. DesiredStateKnown distinguishes a deliberate
	// stopped state from registries written by Agents predating this field.
	DesiredRunning    bool   `json:"desired_running"`
	DesiredStateKnown bool   `json:"desired_state_known,omitempty"`
	RCONHost          string `json:"rcon_host,omitempty"`
	RCONPort          int    `json:"rcon_port,omitempty"`
	TVPort            int    `json:"tv_port,omitempty"`
	// ExternalPID marks an imported server that was already running outside
	// the Agent. Such an instance remains read-only until restart transfers
	// process ownership to the Agent.
	ExternalPID int32     `json:"external_pid,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Registry struct {
	mu      sync.RWMutex
	path    string
	records map[string]InstanceRecord
}

func OpenRegistry(dataDir string) (*Registry, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	r := &Registry{path: filepath.Join(dataDir, "instances.json"), records: make(map[string]InstanceRecord)}
	raw, err := os.ReadFile(r.path)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &r.records); err != nil {
			return nil, err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	legacyMigrated := false
	for id, record := range r.records {
		if record.RelativePath != "" && !filepath.IsAbs(record.RelativePath) {
			delete(r.records, id)
			legacyMigrated = true
			continue
		}
		if record.DesiredStateKnown {
			continue
		}
		// Older registries only persisted AutoRestart. Preserve their recovery
		// behavior on upgrade; all new writes carry explicit desired state.
		record.DesiredRunning = record.AutoRestart
		record.DesiredStateKnown = true
		r.records[id] = record
		legacyMigrated = true
	}
	if err := r.validateUniquePathsLocked(); err != nil {
		return nil, err
	}
	if legacyMigrated {
		if err := r.persistLocked(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Get(id string) (InstanceRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.records[id]
	v.Args = append([]string(nil), v.Args...)
	return v, ok
}

func (r *Registry) List() []InstanceRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]InstanceRecord, 0, len(r.records))
	for _, v := range r.records {
		v.Args = append([]string(nil), v.Args...)
		result = append(result, v)
	}
	return result
}

func (r *Registry) Put(record InstanceRecord) error {
	if record.ID == "" {
		return errors.New("instance id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ownerID, conflict := r.conflictingPathOwnerLocked(record.ID, record); conflict {
		return fmt.Errorf(
			"%w: root_index=%d relative_path=%q is owned by instance %q",
			ErrInstancePathRegistered,
			record.RootIndex,
			record.RelativePath,
			ownerID,
		)
	}
	record.DesiredStateKnown = true
	old, exists := r.records[record.ID]
	now := time.Now().UTC()
	if exists && record.CreatedAt.IsZero() {
		record.CreatedAt = old.CreatedAt
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	r.records[record.ID] = record
	if err := r.persistLocked(); err != nil {
		if exists {
			r.records[record.ID] = old
		} else {
			delete(r.records, record.ID)
		}
		return err
	}
	return nil
}

func (r *Registry) validateUniquePathsLocked() error {
	for id, record := range r.records {
		if ownerID, conflict := r.conflictingPathOwnerLocked(id, record); conflict {
			return fmt.Errorf(
				"%w: root_index=%d relative_path=%q is shared by instances %q and %q",
				ErrInstancePathRegistered,
				record.RootIndex,
				record.RelativePath,
				id,
				ownerID,
			)
		}
	}
	return nil
}

func (r *Registry) conflictingPathOwnerLocked(excludeID string, candidate InstanceRecord) (string, bool) {
	for id, existing := range r.records {
		if id == excludeID {
			continue
		}
		if sameInstancePathForOS(
			candidate.RootIndex,
			candidate.RelativePath,
			existing.RootIndex,
			existing.RelativePath,
			runtime.GOOS,
		) {
			return id, true
		}
	}
	return "", false
}

// sameInstancePathForOS compares paths using the filesystem semantics used by
// the supported Agent platform. Keeping the platform parameter explicit makes
// both Windows and Linux behavior testable on either CI host.
func sameInstancePathForOS(rootA int, relativeA string, rootB int, relativeB string, goos string) bool {
	// A blank path is used by legacy/incomplete records and does not identify a
	// directory. Create/import validation supplies a non-empty relative path.
	if relativeA == "" || relativeB == "" {
		return false
	}
	a := normalizeInstanceRelativePath(relativeA, goos)
	b := normalizeInstanceRelativePath(relativeB, goos)
	absA, absB := instancePathIsAbsolute(a, goos), instancePathIsAbsolute(b, goos)
	if absA != absB || (!absA && rootA != rootB) {
		return false
	}
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func instancePathIsAbsolute(value, goos string) bool {
	if goos == "windows" {
		value = strings.ReplaceAll(value, `\`, "/")
		return len(value) >= 3 && value[1] == ':' && value[2] == '/'
	}
	return strings.HasPrefix(value, "/")
}

func normalizeInstanceRelativePath(relative, goos string) string {
	if goos == "windows" {
		// Windows accepts both separators. path.Clean gives us deterministic
		// lexical normalization even while Windows semantics are tested on Unix.
		relative = strings.ReplaceAll(relative, `\`, "/")
	}
	return path.Clean(relative)
}

func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.records[id]
	if !ok {
		return nil
	}
	delete(r.records, id)
	if err := r.persistLocked(); err != nil {
		r.records[id] = old
		return err
	}
	return nil
}

func (r *Registry) persistLocked() error {
	raw, err := json.MarshalIndent(r.records, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".instances-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o600)
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, r.path)
}
