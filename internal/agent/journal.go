package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

var ErrJobIDConflict = errors.New("job id was already used for different work")

type JournalEntry struct {
	JobID       string           `json:"job_id"`
	Fingerprint string           `json:"fingerprint"`
	Type        string           `json:"type"`
	InstanceID  string           `json:"instance_id,omitempty"`
	Status      domain.JobStatus `json:"status"`
	Progress    int              `json:"progress"`
	Message     string           `json:"message,omitempty"`
	Result      map[string]any   `json:"result,omitempty"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

type Journal struct {
	mu      sync.Mutex
	path    string
	entries map[string]JournalEntry
}

func OpenJournal(dataDir string) (*Journal, error) {
	dir := filepath.Join(dataDir, "journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{path: filepath.Join(dir, "jobs.json"), entries: make(map[string]JournalEntry)}
	raw, err := os.ReadFile(j.path)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &j.entries); err != nil {
			return nil, fmt.Errorf("decode job journal: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	changed := false
	for id, entry := range j.entries {
		if entry.Status == domain.JobRunning || entry.Status == domain.JobDispatched {
			entry.Status = domain.JobFailed
			entry.Message = "agent restarted while the operation was running"
			entry.UpdatedAt = time.Now().UTC()
			j.entries[id] = entry
			changed = true
		}
	}
	if changed {
		if err := j.persistLocked(); err != nil {
			return nil, err
		}
	}
	return j, nil
}

func fingerprintJob(job domain.Job) (string, error) {
	stable := struct {
		ID         string         `json:"id"`
		InstanceID string         `json:"instance_id"`
		Type       string         `json:"type"`
		Payload    map[string]any `json:"payload"`
	}{job.ID, job.InstanceID, job.Type, job.Payload}
	raw, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Begin atomically claims a job. A non-nil existing entry means the caller
// must replay it instead of executing the operation again.
func (j *Journal) Begin(job domain.Job) (*JournalEntry, error) {
	if job.ID == "" {
		return nil, errors.New("job id is required")
	}
	fp, err := fingerprintJob(job)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if old, ok := j.entries[job.ID]; ok {
		if old.Fingerprint != fp {
			return nil, ErrJobIDConflict
		}
		copy := old
		return &copy, nil
	}
	entry := JournalEntry{JobID: job.ID, Fingerprint: fp, Type: job.Type, InstanceID: job.InstanceID, Status: domain.JobDispatched, UpdatedAt: time.Now().UTC()}
	j.entries[job.ID] = entry
	if err := j.persistLocked(); err != nil {
		// Keep the in-memory claim even when durability cannot be confirmed.
		// The atomic replacement may already have committed before a directory
		// flush failed; forgetting the claim here could execute the same job twice
		// in this process. A restart may retry only when the claim truly was not
		// committed, which is the safer failure mode.
		return nil, err
	}
	return nil, nil
}

func (j *Journal) Update(jobID string, status domain.JobStatus, progress int, message string, result map[string]any) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[jobID]
	if !ok {
		return errors.New("job is not in journal")
	}
	if isFinalJobStatus(entry.Status) {
		return nil
	}
	entry.Status, entry.Progress, entry.Message, entry.UpdatedAt = status, progress, message, time.Now().UTC()
	if result != nil {
		entry.Result = result
	}
	j.entries[jobID] = entry
	return j.persistLocked()
}

func (j *Journal) Get(jobID string) (JournalEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[jobID]
	return entry, ok
}

// Acknowledge removes a terminal entry only after the control plane confirms
// that its final result has been durably stored. Until then, reconnects replay
// the entry and the job ID prevents the operation from running twice.
func (j *Journal) Acknowledge(jobID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[jobID]
	if !ok {
		return nil
	}
	if !isFinalJobStatus(entry.Status) {
		return errors.New("cannot acknowledge a non-terminal journal entry")
	}
	delete(j.entries, jobID)
	if err := j.persistLocked(); err != nil {
		j.entries[jobID] = entry
		return err
	}
	return nil
}

func (j *Journal) FinalEntries(limit int) []JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	entries := make([]JournalEntry, 0, len(j.entries))
	for _, entry := range j.entries {
		if isFinalJobStatus(entry.Status) {
			entries = append(entries, entry)
		}
	}
	// Replay the oldest durable outcomes first. Together with the Agent's
	// ACK-driven refill loop this prevents a stream of newer completions from
	// starving an older outcome which may still hold a Control-plane write lock.
	sort.Slice(entries, func(i, k int) bool {
		if entries[i].UpdatedAt.Equal(entries[k].UpdatedAt) {
			return entries[i].JobID < entries[k].JobID
		}
		return entries[i].UpdatedAt.Before(entries[k].UpdatedAt)
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries
}

func isFinalJobStatus(s domain.JobStatus) bool {
	return s == domain.JobSucceeded || s == domain.JobFailed || s == domain.JobCanceled
}

func (j *Journal) persistLocked() error {
	raw, err := json.MarshalIndent(j.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".jobs-*")
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
	return replaceJournalFile(name, j.path)
}
