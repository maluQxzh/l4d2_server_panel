package control

import (
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestValidJobTransition(t *testing.T) {
	valid := [][2]domain.JobStatus{
		{domain.JobQueued, domain.JobDispatched},
		{domain.JobQueued, domain.JobRunning},
		{domain.JobQueued, domain.JobCanceled},
		{domain.JobDispatched, domain.JobRunning},
		{domain.JobDispatched, domain.JobSucceeded},
		{domain.JobRunning, domain.JobSucceeded},
		{domain.JobRunning, domain.JobFailed},
	}
	for _, pair := range valid {
		if !ValidJobTransition(pair[0], pair[1]) {
			t.Errorf("expected %s -> %s to be valid", pair[0], pair[1])
		}
	}
	invalid := [][2]domain.JobStatus{
		{domain.JobQueued, domain.JobSucceeded},
		{domain.JobSucceeded, domain.JobRunning},
		{domain.JobCanceled, domain.JobQueued},
	}
	for _, pair := range invalid {
		if ValidJobTransition(pair[0], pair[1]) {
			t.Errorf("expected %s -> %s to be invalid", pair[0], pair[1])
		}
	}
}

func TestOnlyCancelPendingJobsAcceptLateAgentTerminalOutcome(t *testing.T) {
	t.Parallel()
	ordinary := domain.Job{Status: domain.JobCanceled, Result: map[string]any{}}
	for _, status := range []domain.JobStatus{domain.JobSucceeded, domain.JobFailed} {
		if validPersistedJobTransition(ordinary, status) {
			t.Fatalf("ordinary canceled job was revived as %s", status)
		}
	}
	pending := domain.Job{Status: domain.JobCanceled, Result: map[string]any{"cancel_pending": true}}
	for _, status := range []domain.JobStatus{domain.JobSucceeded, domain.JobFailed, domain.JobCanceled} {
		if !validPersistedJobTransition(pending, status) {
			t.Fatalf("cancel-pending job rejected terminal Agent outcome %s", status)
		}
	}
	if validPersistedJobTransition(pending, domain.JobRunning) || validPersistedJobTransition(pending, domain.JobQueued) {
		t.Fatal("cancel-pending job accepted a non-terminal late update")
	}
}

func TestPendingCancellationClassifiesLateNonTerminalUpdate(t *testing.T) {
	t.Parallel()
	pending := domain.Job{Status: domain.JobCanceled, Result: map[string]any{"cancel_pending": true}}
	if !latePendingCancellationUpdate(pending, domain.JobRunning) {
		t.Fatal("late running update was not classified as stale during pending cancellation")
	}
	for _, status := range []domain.JobStatus{domain.JobSucceeded, domain.JobFailed, domain.JobCanceled} {
		if latePendingCancellationUpdate(pending, status) {
			t.Fatalf("terminal outcome %s was incorrectly ignored", status)
		}
	}
	ordinary := domain.Job{Status: domain.JobCanceled, Result: map[string]any{}}
	if latePendingCancellationUpdate(ordinary, domain.JobRunning) {
		t.Fatal("ordinary canceled job was treated as a pending cancellation")
	}
}
