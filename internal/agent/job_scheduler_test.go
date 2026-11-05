package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestJobSchedulerBoundsQueueDuplicatesAndBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan string, 2)
	release := make(chan struct{})
	s := newJobScheduler(ctx, 1, 1, func(j domain.Job) {
		entered <- j.ID
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	defer func() { cancel(); s.wg.Wait() }()
	j := domain.Job{ID: "one", Type: "backup"}
	if err := s.submit(j, 100); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if err := s.submit(j, 100); err != nil {
		t.Fatal(err)
	}
	changed := j
	changed.Type = "stop"
	if !errors.Is(s.submit(changed, 100), ErrJobIDConflict) {
		t.Fatal("changed duplicate accepted")
	}
	if err := s.submit(domain.Job{ID: "two"}, 100); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.submit(domain.Job{ID: "three"}, 1), errJobQueueFull) {
		t.Fatal("queue is unbounded")
	}
	a, q, b := s.stats()
	if a != 1 || q != 1 || b != 200 {
		t.Fatalf("stats: %d %d %d", a, q, b)
	}
	close(release)
	select {
	case id := <-entered:
		if id != "two" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("queued job lost")
	}
	cancel()
	s.wg.Wait()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := newJobScheduler(ctx2, 1, 1, func(domain.Job) { <-ctx2.Done() })
	defer func() { cancel2(); s2.wg.Wait() }()
	if err := s2.submit(domain.Job{ID: "large"}, maxResidentJobBytes); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s2.submit(domain.Job{ID: "overflow"}, 1), errJobQueueFull) {
		t.Fatal("byte budget not enforced")
	}
}

func TestQueuedJobCancellationRemainsDurable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{ctx: ctx, config: &Config{NodeID: "node"}, journal: j, logger: log.New(io.Discard, "", 0), activeJobs: make(map[string]context.CancelFunc), pendingCancels: make(map[string]bool)}
	entered := make(chan struct{})
	release := make(chan struct{})
	var executions atomic.Int32
	s := newJobScheduler(ctx, 1, 1, func(job domain.Job) {
		a.handleJobWithExecutor(job, func(ctx context.Context, job domain.Job, _ ProgressFunc) (map[string]any, error) {
			executions.Add(1)
			if job.ID == "first" {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return nil, ctx.Err()
		})
	}, a.acknowledgeJob)
	defer func() { cancel(); s.wg.Wait() }()
	if err := s.submit(domain.Job{ID: "first", NodeID: "node", Type: "backup"}, 1); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := s.submit(domain.Job{ID: "waiting", NodeID: "node", Type: "backup"}, 1); err != nil {
		t.Fatal(err)
	}
	a.cancelJob("waiting")
	if !s.deferAcknowledgement("waiting") {
		t.Fatal("queued cancellation ACK was not deferred")
	}
	entry, ok := j.Get("waiting")
	if !ok || entry.Status != domain.JobCanceled {
		t.Fatal("cancellation not durable")
	}
	close(release)
	deadline := time.After(2 * time.Second)
	for {
		active, queued, _ := s.stats()
		if active == 0 && queued == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("queue did not drain")
		case <-time.After(time.Millisecond):
		}
	}
	if executions.Load() != 1 {
		t.Fatal("canceled queued job executed")
	}
	if _, exists := j.Get("waiting"); exists {
		t.Fatal("deferred ACK did not clear observed cancellation")
	}
}
