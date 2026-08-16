package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

var ErrJobIDConflict = errors.New("job id was already used for different work")
var ErrJobNotInJournal = errors.New("job is not in journal")

const (
	cancellationOnlyJournalType = "__cancel_before_dispatch"
	journalV2DirectoryName      = "v2"
	legacyJournalName           = "jobs.json"
	legacyJournalArchiveName    = "jobs.v1.migrated.json"
	maxJournalEntries           = 4096
	maxJournalEntryBytes        = 16 << 20
	maxLegacyJournalBytes       = 1 << 30
)

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
	mu         sync.Mutex
	dir        string
	legacyPath string
	entries    map[string]JournalEntry
}

func OpenJournal(dataDir string) (*Journal, error) {
	root := filepath.Join(dataDir, "journal")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, journalV2DirectoryName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for _, path := range []string{root, dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || isReparseOrSymlink(info) {
			return nil, fmt.Errorf("job journal directory is not a safe local directory: %s", path)
		}
		_ = os.Chmod(path, 0o700)
	}
	j := &Journal{dir: dir, legacyPath: filepath.Join(root, legacyJournalName), entries: make(map[string]JournalEntry)}
	if err := j.loadV2(); err != nil {
		return nil, err
	}
	if err := j.migrateLegacy(); err != nil {
		return nil, err
	}
	for id, entry := range j.entries {
		next := entry
		changed := false
		if next.Status == domain.JobRunning || next.Status == domain.JobDispatched {
			next.Status = domain.JobFailed
			next.Message = "agent restarted while the operation was running"
			next.UpdatedAt = time.Now().UTC()
			changed = true
		}
		normalizedMessage := protocol.TruncateJobUpdateMessage(next.Message)
		if normalizedMessage != next.Message {
			next.Message = normalizedMessage
			changed = true
		}
		if changed {
			if err := j.persistEntryLocked(next); err != nil {
				return nil, err
			}
			j.entries[id] = next
		}
	}
	return j, nil
}

func (j *Journal) loadV2() error {
	items, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	if len(items) > maxJournalEntries*2 {
		return errors.New("job journal contains too many files")
	}
	for _, item := range items {
		name := item.Name()
		path := filepath.Join(j.dir, name)
		if strings.HasPrefix(name, ".job-") {
			info, statErr := os.Lstat(path)
			if statErr == nil && info.Mode().IsRegular() && !isReparseOrSymlink(info) {
				if err := removeJournalFile(path, j.dir); err != nil {
					return fmt.Errorf("remove incomplete job journal entry %q: %w", name, err)
				}
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") || len(name) != sha256.Size*2+len(".json") {
			return fmt.Errorf("job journal contains an unexpected entry %q", name)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) || info.Size() <= 0 || info.Size() > maxJournalEntryBytes {
			return fmt.Errorf("job journal entry %q is not a safe bounded regular file", name)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var entry JournalEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("decode job journal entry %q: %w", name, err)
		}
		if err := validateJournalEntry(entry); err != nil {
			return fmt.Errorf("validate job journal entry %q: %w", name, err)
		}
		if name != journalEntryFileName(entry.JobID) {
			return fmt.Errorf("job journal entry %q does not match its job ID", name)
		}
		if _, duplicate := j.entries[entry.JobID]; duplicate {
			return fmt.Errorf("job journal contains duplicate job ID %q", entry.JobID)
		}
		j.entries[entry.JobID] = entry
	}
	if len(j.entries) > maxJournalEntries {
		return errors.New("job journal contains too many entries")
	}
	return nil
}

func (j *Journal) migrateLegacy() error {
	raw, err := os.ReadFile(j.legacyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > maxLegacyJournalBytes {
		return errors.New("legacy job journal is too large")
	}
	legacy := make(map[string]JournalEntry)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return fmt.Errorf("decode legacy job journal: %w", err)
		}
	}
	if len(legacy) > maxJournalEntries {
		return errors.New("legacy job journal contains too many entries")
	}
	for id, entry := range legacy {
		if entry.JobID == "" {
			entry.JobID = id
		}
		if entry.JobID != id {
			return fmt.Errorf("legacy job journal key does not match job ID %q", id)
		}
		if err := validateJournalEntry(entry); err != nil {
			return fmt.Errorf("validate legacy job journal entry %q: %w", id, err)
		}
		if current, exists := j.entries[id]; exists {
			currentRaw, _ := json.Marshal(current)
			legacyRaw, _ := json.Marshal(entry)
			if !bytes.Equal(currentRaw, legacyRaw) {
				return fmt.Errorf("legacy and v2 job journal entries disagree for %q", id)
			}
			continue
		}
		if len(j.entries) >= maxJournalEntries {
			return errors.New("job journal contains too many entries after migration")
		}
		if err := j.persistEntryLocked(entry); err != nil {
			return fmt.Errorf("migrate legacy job journal entry %q: %w", id, err)
		}
		j.entries[id] = entry
	}
	archive := filepath.Join(filepath.Dir(j.legacyPath), legacyJournalArchiveName)
	if _, err := os.Lstat(archive); err == nil {
		return errors.New("legacy job journal archive already exists while migration source remains")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := replaceJournalFile(j.legacyPath, archive); err != nil {
		return fmt.Errorf("archive migrated legacy job journal: %w", err)
	}
	return nil
}

func validateJournalEntry(entry JournalEntry) error {
	if entry.JobID == "" || len(entry.JobID) > 200 {
		return errors.New("job ID is invalid")
	}
	if entry.Progress < 0 || entry.Progress > 100 {
		return errors.New("job progress is invalid")
	}
	switch entry.Status {
	case domain.JobDispatched, domain.JobRunning, domain.JobSucceeded, domain.JobFailed, domain.JobCanceled:
	default:
		return errors.New("job status is invalid")
	}
	if len(entry.Message) > protocol.MaxJobUpdateMessageBytes*2 {
		return errors.New("job message is unreasonably large")
	}
	return nil
}

func journalEntryFileName(jobID string) string {
	sum := sha256.Sum256([]byte(jobID))
	return hex.EncodeToString(sum[:]) + ".json"
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
		if old.Type == cancellationOnlyJournalType && old.Status == domain.JobCanceled {
			copy := old
			return &copy, nil
		}
		if old.Fingerprint != fp {
			return nil, ErrJobIDConflict
		}
		copy := old
		return &copy, nil
	}
	if len(j.entries) >= maxJournalEntries {
		return nil, errors.New("job journal is full")
	}
	entry := JournalEntry{JobID: job.ID, Fingerprint: fp, Type: job.Type, InstanceID: job.InstanceID, Status: domain.JobDispatched, UpdatedAt: time.Now().UTC()}
	if err := j.persistEntryLocked(entry); err != nil {
		return nil, err
	}
	j.entries[job.ID] = entry
	return nil, nil
}

func (j *Journal) Update(jobID string, status domain.JobStatus, progress int, message string, result map[string]any) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	message = protocol.TruncateJobUpdateMessage(message)
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[jobID]
	if !ok {
		return ErrJobNotInJournal
	}
	if isFinalJobStatus(entry.Status) {
		return nil
	}
	entry.Status, entry.Progress, entry.Message, entry.UpdatedAt = status, progress, message, time.Now().UTC()
	if result != nil {
		entry.Result = result
	}
	if err := j.persistEntryLocked(entry); err != nil {
		return err
	}
	j.entries[jobID] = entry
	return nil
}

func (j *Journal) CancelBeforeDispatch(jobID, message string) error {
	if jobID == "" {
		return errors.New("job id is required")
	}
	message = protocol.TruncateJobUpdateMessage(message)
	j.mu.Lock()
	defer j.mu.Unlock()
	if entry, ok := j.entries[jobID]; ok {
		if isFinalJobStatus(entry.Status) {
			return nil
		}
		return errors.New("cannot replace a claimed job with a cancellation tombstone")
	}
	if len(j.entries) >= maxJournalEntries {
		return errors.New("job journal is full")
	}
	entry := JournalEntry{JobID: jobID, Type: cancellationOnlyJournalType, Status: domain.JobCanceled,
		Message: message, UpdatedAt: time.Now().UTC()}
	if err := j.persistEntryLocked(entry); err != nil {
		return err
	}
	j.entries[jobID] = entry
	return nil
}

func (j *Journal) Get(jobID string) (JournalEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[jobID]
	return entry, ok
}

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
	if err := removeJournalFile(filepath.Join(j.dir, journalEntryFileName(jobID)), j.dir); err != nil {
		return err
	}
	delete(j.entries, jobID)
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

func (j *Journal) persistEntryLocked(entry JournalEntry) error {
	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxJournalEntryBytes {
		return errors.New("job journal entry exceeds the size limit")
	}
	tmp, err := os.CreateTemp(j.dir, ".job-*")
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
	return replaceJournalFile(name, filepath.Join(j.dir, journalEntryFileName(entry.JobID)))
}

func (j *Journal) persistLocked() error {
	for _, entry := range j.entries {
		if err := j.persistEntryLocked(entry); err != nil {
			return err
		}
	}
	return nil
}
