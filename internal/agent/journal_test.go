package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestJournalClaimsAndReplaysJobIdempotently(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "job-1", InstanceID: "instance-1", Type: "start", Payload: map[string]any{"port": 27015}}
	if old, err := j.Begin(job); err != nil || old != nil {
		t.Fatalf("first Begin = %#v, %v", old, err)
	}
	if err := j.Update(job.ID, domain.JobRunning, 50, "running", nil); err != nil {
		t.Fatal(err)
	}
	if old, err := j.Begin(job); err != nil || old == nil || old.Status != domain.JobRunning {
		t.Fatalf("duplicate Begin = %#v, %v", old, err)
	}
	conflict := job
	conflict.Payload = map[string]any{"port": 27016}
	if _, err := j.Begin(conflict); !errors.Is(err, ErrJobIDConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestJournalMarksInterruptedWorkFailed(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "job-restart", InstanceID: "instance-1", Type: "update"}
	if _, err := j.Begin(job); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(job.ID, domain.JobRunning, 20, "updating", nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Get(job.ID)
	if !ok || entry.Status != domain.JobFailed {
		t.Fatalf("entry after restart = %#v, %t", entry, ok)
	}
}

func TestJournalNormalizesLegacyOversizedMessageOnOpen(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyMessage := strings.Repeat("x", protocol.MaxJobUpdateMessageBytes-1) + "界"
	entries := map[string]JournalEntry{"legacy-job": {
		JobID: "legacy-job", Status: domain.JobFailed, Progress: 5, Message: legacyMessage, UpdatedAt: time.Now().UTC(),
	}}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(journalDir, "jobs.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := journal.Get("legacy-job")
	if !ok {
		t.Fatal("legacy journal entry disappeared")
	}
	if len(entry.Message) > protocol.MaxJobUpdateMessageBytes || !utf8.ValidString(entry.Message) {
		t.Fatalf("normalized message is %d bytes, valid UTF-8=%t", len(entry.Message), utf8.ValidString(entry.Message))
	}

	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reopened.Get("legacy-job")
	if !ok || persisted.Message != entry.Message {
		t.Fatalf("normalized message was not persisted: %#v", persisted)
	}
}

func TestJournalUpdateTruncatesMessageAtUTF8Boundary(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "bounded-message", Type: "test"}
	if _, err := journal.Begin(job); err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("x", protocol.MaxJobUpdateMessageBytes-1) + "界"
	if err := journal.Update(job.ID, domain.JobFailed, 5, message, nil); err != nil {
		t.Fatal(err)
	}
	entry, ok := journal.Get(job.ID)
	if !ok || len(entry.Message) > protocol.MaxJobUpdateMessageBytes || !utf8.ValidString(entry.Message) {
		t.Fatalf("journal stored an invalid bounded message: %#v", entry)
	}
}

func TestJournalUpdateFailureDoesNotExposeUncommittedTerminalOutcome(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "terminal-write-failure", Type: "test"}
	if _, err := journal.Begin(job); err != nil {
		t.Fatal(err)
	}
	journal.path = t.TempDir() // Replacing a directory with the journal file must fail on every supported OS.
	if err := journal.Update(job.ID, domain.JobSucceeded, 100, "done", map[string]any{"secret": "must-not-surface"}); err == nil {
		t.Fatal("terminal journal update unexpectedly succeeded")
	}
	entry, ok := journal.Get(job.ID)
	if !ok || entry.Status != domain.JobDispatched || entry.Progress != 0 || entry.Message != "" || entry.Result != nil {
		t.Fatalf("uncommitted terminal outcome remained visible in memory: %#v present=%t", entry, ok)
	}
}

func TestJournalClaimFailureRollsBackInMemoryClaim(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journal.path = t.TempDir()
	job := domain.Job{ID: "claim-write-failure", Type: "test"}
	if _, err := journal.Begin(job); err == nil {
		t.Fatal("journal claim unexpectedly succeeded")
	}
	if _, ok := journal.Get(job.ID); ok {
		t.Fatal("failed journal claim remained visible in memory")
	}
}

func TestJournalAcknowledgementRemovesOnlyTerminalEntry(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "job-ack", InstanceID: "instance-1", Type: "backup.chunk"}
	if _, err := j.Begin(job); err != nil {
		t.Fatal(err)
	}
	if err := j.Acknowledge(job.ID); err == nil {
		t.Fatal("running journal entry was acknowledged")
	}
	if err := j.Update(job.ID, domain.JobSucceeded, 100, "done", map[string]any{"content_base64": "large-result"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Acknowledge(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Get(job.ID); ok {
		t.Fatal("acknowledged terminal entry remained in memory")
	}
	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get(job.ID); ok {
		t.Fatal("acknowledged terminal entry remained on disk")
	}
	if err := reopened.Acknowledge(job.ID); err != nil {
		t.Fatalf("duplicate acknowledgement was not idempotent: %v", err)
	}
}

func TestTerminalJournalACKDrivenReplayDrainsMoreThanTwoPages(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const total = 501
	base := time.Now().UTC().Add(-time.Hour)
	j.mu.Lock()
	for index := 0; index < total; index++ {
		jobID := fmt.Sprintf("job-%03d", index)
		j.entries[jobID] = JournalEntry{
			JobID:     jobID,
			Status:    domain.JobSucceeded,
			Progress:  100,
			Message:   "done",
			UpdatedAt: base.Add(time.Duration(index) * time.Millisecond),
		}
	}
	err = j.persistLocked()
	j.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	agent := &Agent{
		config:  &Config{NodeID: "node-1"},
		journal: j,
		logger:  log.New(io.Discard, "", 0),
	}
	sender := make(chan protocol.Envelope, 512)
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go agent.replayTerminalJournal(ctx, sender, wake)
	wake <- struct{}{}

	// Let replay establish a real backlog before injecting ordinary traffic.
	// A dedicated replay producer must not prevent that traffic from entering
	// and leaving the shared outbound queue while later pages are ACKed.
	var first protocol.Envelope
	select {
	case first = <-sender:
	case <-ctx.Done():
		t.Fatal("timed out waiting for initial terminal replay page")
	}
	if first.Type != protocol.TypeJobUpdate {
		t.Fatalf("first envelope type = %q, want %q", first.Type, protocol.TypeJobUpdate)
	}
	if err := j.Acknowledge(first.JobID); err != nil {
		t.Fatal(err)
	}
	wake <- struct{}{}
	heartbeat, err := protocol.NewEnvelope(protocol.TypeHeartbeat, "node-1", "", protocol.Heartbeat{})
	if err != nil {
		t.Fatal(err)
	}
	sender <- heartbeat

	seen := map[string]bool{first.JobID: true}
	heartbeatSeen := false
	for len(seen) < total || !heartbeatSeen {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out after %d/%d unique terminal outcomes (heartbeat=%t)", len(seen), total, heartbeatSeen)
		case envelope := <-sender:
			switch envelope.Type {
			case protocol.TypeHeartbeat:
				heartbeatSeen = true
				if len(seen) >= total {
					t.Fatal("heartbeat was blocked until the entire terminal backlog drained")
				}
			case protocol.TypeJobUpdate:
				seen[envelope.JobID] = true
				if err := j.Acknowledge(envelope.JobID); err != nil {
					t.Fatal(err)
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			default:
				t.Fatalf("unexpected envelope type %q", envelope.Type)
			}
		}
	}
	if remaining := j.FinalEntries(1); len(remaining) != 0 {
		t.Fatalf("terminal journal did not drain: %#v", remaining)
	}
}
