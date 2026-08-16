package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func TestHandleClaimedJobLateCancelPreservesCommittedSuccess(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "late-cancel", NodeID: "node-1", Type: "test.commit"}
	if existing, err := journal.Begin(job); err != nil || existing != nil {
		t.Fatalf("claim test job: existing=%v err=%v", existing, err)
	}
	agent := &Agent{
		config:         &Config{NodeID: "node-1"},
		journal:        journal,
		ctx:            context.Background(),
		activeJobs:     make(map[string]context.CancelFunc),
		pendingCancels: make(map[string]bool),
	}

	committed := make(chan struct{})
	releaseExecutor := make(chan struct{})
	executorContext := make(chan error, 1)
	done := make(chan struct{})
	wantResult := map[string]any{"committed": true, "revision": "42"}
	go func() {
		defer close(done)
		agent.handleClaimedJob(job, func(ctx context.Context, _ domain.Job, _ ProgressFunc) (map[string]any, error) {
			// The operation is already durably committed at this point. Hold the
			// return so the test can deliver a cancellation in the exact race
			// window that previously discarded a successful result.
			close(committed)
			<-releaseExecutor
			executorContext <- ctx.Err()
			return wantResult, nil
		})
	}()

	waitForSignal(t, committed, "executor commit")
	agent.cancelJob(job.ID)
	close(releaseExecutor)
	waitForSignal(t, done, "job completion")
	if err := <-executorContext; !errors.Is(err, context.Canceled) {
		t.Fatalf("executor context = %v, want canceled", err)
	}

	entry, ok := journal.Get(job.ID)
	if !ok {
		t.Fatal("completed job disappeared from journal")
	}
	if entry.Status != domain.JobSucceeded || entry.Progress != 100 {
		t.Fatalf("completion = %s/%d, want succeeded/100", entry.Status, entry.Progress)
	}
	if !reflect.DeepEqual(entry.Result, wantResult) {
		t.Fatalf("result = %#v, want %#v", entry.Result, wantResult)
	}
}

func TestExecutionCompletionUsesExecutorErrorOnly(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want domain.JobStatus
	}{
		{name: "success", want: domain.JobSucceeded},
		{name: "canceled", err: context.Canceled, want: domain.JobCanceled},
		{name: "wrapped deadline", err: fmt.Errorf("executor stopped: %w", context.DeadlineExceeded), want: domain.JobFailed},
		{name: "ordinary failure", err: errors.New("steamcmd failed"), want: domain.JobFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, _ := executionCompletion(test.err)
			if status != test.want {
				t.Fatalf("status = %s, want %s", status, test.want)
			}
		})
	}
}

func TestHandleClaimedJobDoesNotSendUnpersistedTerminalOutcome(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "unpersisted-terminal", NodeID: "node-1", Type: "test.commit"}
	if _, err := journal.Begin(job); err != nil {
		t.Fatal(err)
	}
	journal.dir = filepath.Join(t.TempDir(), "missing", "v2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sender := make(chan protocol.Envelope, 4)
	agent := &Agent{config: &Config{NodeID: "node-1"}, journal: journal, logger: log.New(io.Discard, "", 0),
		ctx: ctx, sender: sender, activeJobs: make(map[string]context.CancelFunc), pendingCancels: make(map[string]bool)}

	agent.handleClaimedJob(job, func(context.Context, domain.Job, ProgressFunc) (map[string]any, error) {
		return map[string]any{"secret": "must-not-send"}, nil
	})
	select {
	case envelope := <-sender:
		t.Fatalf("unpersisted journal state produced an outbound update: %#v", envelope)
	default:
	}
	entry, ok := journal.Get(job.ID)
	if !ok || entry.Status != domain.JobDispatched || entry.Result != nil {
		t.Fatalf("journal exposed an unpersisted terminal outcome: %#v present=%t", entry, ok)
	}
}

func TestHandleClaimedJobRetriesAndSendsExactPersistedTerminalOutcome(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "retried-terminal", NodeID: "node-1", Type: "test.commit"}
	if _, err := journal.Begin(job); err != nil {
		t.Fatal(err)
	}
	validDir := journal.dir
	journal.dir = filepath.Join(t.TempDir(), "missing", "v2")
	failureSeen := make(chan struct{}, 1)
	logger := log.New(terminalPersistenceSignalWriter{signal: failureSeen}, "", 0)
	sender := make(chan protocol.Envelope, 4)
	agent := &Agent{config: &Config{NodeID: "node-1"}, journal: journal, logger: logger,
		ctx: context.Background(), sender: sender, activeJobs: make(map[string]context.CancelFunc), pendingCancels: make(map[string]bool)}
	wantResult := map[string]any{"revision": "42", "committed": true}
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.handleClaimedJob(job, func(context.Context, domain.Job, ProgressFunc) (map[string]any, error) {
			return wantResult, nil
		})
	}()
	select {
	case <-failureSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal persistence failure was not observed")
	}
	journal.mu.Lock()
	journal.dir = validDir
	journal.mu.Unlock()
	waitForSignal(t, done, "terminal journal retry")

	select {
	case envelope := <-sender:
		var update protocol.JobUpdate
		if err := json.Unmarshal(envelope.Payload, &update); err != nil {
			t.Fatal(err)
		}
		if update.Status != domain.JobSucceeded || update.Progress != 100 || !reflect.DeepEqual(update.Result, wantResult) {
			t.Fatalf("retried terminal update = %#v", update)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("persisted terminal outcome was not sent")
	}
	entry, ok := journal.Get(job.ID)
	if !ok || entry.Status != domain.JobSucceeded || !reflect.DeepEqual(entry.Result, wantResult) {
		t.Fatalf("durable terminal outcome = %#v present=%t", entry, ok)
	}
}

type terminalPersistenceSignalWriter struct {
	signal chan<- struct{}
}

func (w terminalPersistenceSignalWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("status=succeeded")) {
		select {
		case w.signal <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
