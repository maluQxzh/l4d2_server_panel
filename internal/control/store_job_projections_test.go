package control

import "testing"

func TestJobProjectionLockSerializesDuplicateFrames(t *testing.T) {
	t.Parallel()
	store := &Store{}
	releaseFirst := store.lockJobProjection("job-1")
	started := make(chan struct{})
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		release := store.lockJobProjection("job-1")
		close(acquired)
		release()
		close(done)
	}()
	<-started
	select {
	case <-acquired:
		t.Fatal("duplicate projection acquired the same keyed lock concurrently")
	default:
	}
	releaseFirst()
	<-done

	store.projectionLocksMu.Lock()
	defer store.projectionLocksMu.Unlock()
	if len(store.projectionJobLocks) != 0 {
		t.Fatalf("projection lock registry retained %d unused entries", len(store.projectionJobLocks))
	}
}
