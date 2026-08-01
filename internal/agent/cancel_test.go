package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestCancelJobCancelsActiveContext(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{journal: j, activeJobs: make(map[string]context.CancelFunc), pendingCancels: make(map[string]bool)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.activeJobs["job-1"] = cancel
	a.cancelJob("job-1")
	select {
	case <-ctx.Done():
	default:
		t.Fatal("active job context was not canceled")
	}
}

func TestCancelJobReplaysJournalFinalResult(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "job-final", NodeID: "node-1", InstanceID: "instance-1", Type: "restore", Payload: map[string]any{"upload_id": "upload-1"}}
	if existing, err := j.Begin(job); err != nil || existing != nil {
		t.Fatalf("claim final job: existing=%v err=%v", existing, err)
	}
	wantResult := map[string]any{"restored_files": 3, "upload_cleaned": true}
	if err := j.Update(job.ID, domain.JobSucceeded, 100, "restore completed", wantResult); err != nil {
		t.Fatal(err)
	}
	sender := make(chan protocol.Envelope, 1)
	a := &Agent{
		config:         &Config{NodeID: "node-1"},
		journal:        j,
		sender:         sender,
		activeJobs:     make(map[string]context.CancelFunc),
		pendingCancels: make(map[string]bool),
	}

	a.cancelJob(job.ID)
	update := receiveCancelUpdate(t, sender, job.ID)
	if update.Status != domain.JobSucceeded || update.Progress != 100 || update.Message != "restore completed" ||
		update.Result["restored_files"] != float64(3) || update.Result["upload_cleaned"] != true {
		t.Fatalf("replayed final update = %#v", update)
	}
}

func TestCancelUnknownJobAcknowledgesAndPreventsLateExecution(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := make(chan protocol.Envelope, 2)
	a := &Agent{
		config:         &Config{NodeID: "node-1"},
		journal:        j,
		ctx:            context.Background(),
		sender:         sender,
		activeJobs:     make(map[string]context.CancelFunc),
		pendingCancels: make(map[string]bool),
	}
	job := domain.Job{ID: "job-not-delivered", NodeID: "node-1", Type: "backup.upload.chunk", Payload: map[string]any{"offset": 0}}

	a.cancelJob(job.ID)
	update := receiveCancelUpdate(t, sender, job.ID)
	if update.Status != domain.JobCanceled || update.Message != "operation canceled before dispatch" {
		t.Fatalf("unknown-job cancellation = %#v", update)
	}
	if !a.pendingCancels[job.ID] {
		t.Fatal("unknown canceled job did not retain a late-dispatch guard")
	}

	if existing, err := j.Begin(job); err != nil || existing != nil {
		t.Fatalf("claim late job: existing=%v err=%v", existing, err)
	}
	executed := false
	a.handleClaimedJob(job, func(context.Context, domain.Job, ProgressFunc) (map[string]any, error) {
		executed = true
		return nil, nil
	})
	if executed {
		t.Fatal("late job executed after its cancellation was acknowledged")
	}
	update = receiveCancelUpdate(t, sender, job.ID)
	if update.Status != domain.JobCanceled || update.Message != "operation canceled before execution" {
		t.Fatalf("late-job cancellation = %#v", update)
	}
	entry, ok := j.Get(job.ID)
	if !ok || entry.Status != domain.JobCanceled {
		t.Fatalf("late job journal entry = %#v, present=%v", entry, ok)
	}
}

func TestCancelClaimedJobWaitsForClaimedWorker(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "job-claimed", NodeID: "node-1", Type: "restore"}
	if existing, err := j.Begin(job); err != nil || existing != nil {
		t.Fatalf("claim job: existing=%v err=%v", existing, err)
	}
	sender := make(chan protocol.Envelope, 1)
	a := &Agent{
		config:         &Config{NodeID: "node-1"},
		journal:        j,
		ctx:            context.Background(),
		sender:         sender,
		activeJobs:     make(map[string]context.CancelFunc),
		pendingCancels: make(map[string]bool),
	}

	a.cancelJob(job.ID)
	select {
	case envelope := <-sender:
		t.Fatalf("claimed non-terminal job was acknowledged before its worker observed cancellation: %#v", envelope)
	default:
	}
	if !a.pendingCancels[job.ID] {
		t.Fatal("claimed non-terminal job did not retain pending cancellation")
	}
	a.handleClaimedJob(job, func(context.Context, domain.Job, ProgressFunc) (map[string]any, error) {
		t.Fatal("claimed canceled job reached executor")
		return nil, nil
	})
	if update := receiveCancelUpdate(t, sender, job.ID); update.Status != domain.JobCanceled {
		t.Fatalf("claimed job final update = %#v", update)
	}
}

func receiveCancelUpdate(t *testing.T, sender <-chan protocol.Envelope, jobID string) protocol.JobUpdate {
	t.Helper()
	select {
	case envelope := <-sender:
		if envelope.Type != protocol.TypeJobUpdate || envelope.JobID != jobID {
			t.Fatalf("unexpected cancellation envelope: %#v", envelope)
		}
		var update protocol.JobUpdate
		if err := json.Unmarshal(envelope.Payload, &update); err != nil {
			t.Fatalf("decode cancellation update: %v", err)
		}
		return update
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for cancellation update for %s", jobID)
		return protocol.JobUpdate{}
	}
}
