package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const nodeJobWorkers = 8
const nodeJobQueueSize = 24
const maxResidentJobBytes = 256 << 20

var errJobQueueFull = errors.New("node job queue is full; reconnect to replay durable dispatches")

type scheduledJob struct {
	job   domain.Job
	bytes int
}
type jobScheduler struct {
	ctx            context.Context
	mu             sync.Mutex
	queue          chan scheduledJob
	resident       map[string]string
	acks           map[string]bool
	onAcknowledged func(string)
	bytes          int
	active         int
	wg             sync.WaitGroup
}

func newJobScheduler(ctx context.Context, workers, queueSize int, execute func(domain.Job), acknowledged ...func(string)) *jobScheduler {
	s := &jobScheduler{ctx: ctx, queue: make(chan scheduledJob, workers+queueSize), resident: make(map[string]string), acks: make(map[string]bool)}
	if len(acknowledged) > 0 {
		s.onAcknowledged = acknowledged[0]
	}
	for range workers {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case entry := <-s.queue:
					if ctx.Err() != nil {
						return
					}
					s.mu.Lock()
					s.active++
					s.mu.Unlock()
					execute(entry.job)
					s.mu.Lock()
					s.active--
					s.bytes -= entry.bytes
					delete(s.resident, entry.job.ID)
					acknowledged := s.acks[entry.job.ID]
					delete(s.acks, entry.job.ID)
					// Serialize acknowledgement cleanup before the same ID can be
					// admitted again. In particular, retain cancellation tombstones
					// until a queued job has observed them.
					if acknowledged && s.onAcknowledged != nil {
						s.onAcknowledged(entry.job.ID)
					}
					s.mu.Unlock()
				}
			}
		}()
	}
	return s
}

func (s *jobScheduler) deferAcknowledgement(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.resident[id]; !ok {
		return false
	}
	s.acks[id] = true
	return true
}

func (s *jobScheduler) submit(job domain.Job, bytes int) error {
	if job.ID == "" || bytes < 0 {
		return errors.New("invalid job admission")
	}
	fingerprint, err := fingerprintJob(job)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if prior, ok := s.resident[job.ID]; ok {
		if prior != fingerprint {
			return ErrJobIDConflict
		}
		return nil
	}
	if len(s.resident) >= cap(s.queue) || bytes > maxResidentJobBytes-s.bytes {
		return errJobQueueFull
	}
	select {
	case s.queue <- scheduledJob{job: job, bytes: bytes}:
		s.resident[job.ID] = fingerprint
		s.bytes += bytes
		return nil
	default:
		return errJobQueueFull
	}
}

func (s *jobScheduler) stats() (active, queued, bytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, len(s.queue), s.bytes
}
