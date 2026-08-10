package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
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

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
