package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestBackgroundA2SProbeCannotCrossLifecycleTransition(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "a2s-lifecycle.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "probe-target", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	target := monitoringTarget{InstanceID: instance.ID, NodeID: instance.NodeID}
	if _, _, err := store.QueueSystemProbe(ctx, target, "a2s.info", "stopped-probe"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped instance accepted background A2S probe: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='running' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	probe, reused, err := store.QueueSystemProbe(ctx, target, "a2s.info", "running-probe")
	if err != nil || reused {
		t.Fatalf("queue running probe: reused=%v err=%v", reused, err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "stop", map[string]any{}, "", "stop-after-probe"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.MarkDispatched(ctx, probe.ID); err != nil || claimed {
		t.Fatalf("stale probe dispatch: claimed=%v err=%v", claimed, err)
	}
	canceled, err := store.GetJob(ctx, probe.ID)
	if err != nil || canceled.Status != domain.JobCanceled {
		t.Fatalf("stale probe was not settled as canceled: %#v err=%v", canceled, err)
	}
	if _, _, err := store.QueueSystemProbe(ctx, target, "a2s.info", "stopping-probe"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopping instance accepted background A2S probe: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded' WHERE type='stop'; UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "start", map[string]any{}, "", "start-before-probe"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueSystemProbe(ctx, target, "a2s.info", "starting-probe"); !errors.Is(err, ErrConflict) {
		t.Fatalf("starting instance accepted background A2S probe: %v", err)
	}
}

func TestListJobsFiltersBackgroundBeforeLimit(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "jobs.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role)
		VALUES('owner','owner','owner','Owner','hash','owner');
		INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO jobs(id,node_id,type,status,requested_by,created_at) VALUES
			('operation','node','restart','succeeded',NULL,?1),
			('manual-probe','node','a2s.info','succeeded','owner',?2),
			('background','node','a2s.info','succeeded',NULL,?3)`,
		time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC(), time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	operations, err := store.ListJobs(ctx, "", "", jobBackgroundExclude, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != "manual-probe" {
		t.Fatalf("operations = %#v, want newest non-background job", operations)
	}
	background, err := store.ListJobs(ctx, "", "", jobBackgroundOnly, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(background) != 1 || background[0].ID != "background" {
		t.Fatalf("background = %#v, want automatic probe", background)
	}
}

func TestSummarizeBackgroundJobsUsesLatestJobPerTarget(t *testing.T) {
	latest := time.Unix(4, 0).UTC()
	summary := summarizeBackgroundJobs([]domain.Job{
		{ID: "active", NodeID: "node", InstanceID: "instance-a", Status: domain.JobRunning, CreatedAt: latest},
		{ID: "failed", NodeID: "node", InstanceID: "instance-b", Status: domain.JobFailed, CreatedAt: time.Unix(3, 0).UTC()},
		{ID: "stale-failure", NodeID: "node", InstanceID: "instance-a", Status: domain.JobFailed, CreatedAt: time.Unix(2, 0).UTC()},
	})
	if summary.Targets != 2 || summary.Active != 1 || summary.Failed != 1 || summary.LatestAt == nil || !summary.LatestAt.Equal(latest) {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestHousekeepingRetainsBackgroundFailuresLongerThanSuccesses(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "housekeeping.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO jobs(id,node_id,type,status,finished_at,created_at) VALUES
			('old-success','node','a2s.info','succeeded',?1,?1),
			('recent-success','node','a2s.info','succeeded',?2,?2),
			('recent-failure','node','a2s.info','failed',?1,?1),
			('old-failure','node','a2s.info','failed',?3,?3),
			('user-operation','node','restart','succeeded',?1,?1)`,
		now.Add(-2*time.Hour), now.Add(-30*time.Minute), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Housekeeping(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var retained []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		retained = append(retained, id)
	}
	want := []string{"recent-failure", "recent-success", "user-operation"}
	if fmt.Sprint(retained) != fmt.Sprint(want) {
		t.Fatalf("retained jobs = %v, want %v", retained, want)
	}
}

func TestValidJobTransition(t *testing.T) {
	valid := [][2]domain.JobStatus{
		{domain.JobQueued, domain.JobDispatched},
		{domain.JobQueued, domain.JobRunning},
		{domain.JobQueued, domain.JobCanceled},
		{domain.JobQueued, domain.JobSucceeded},
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
		{domain.JobSucceeded, domain.JobRunning},
		{domain.JobCanceled, domain.JobQueued},
	}
	for _, pair := range invalid {
		if ValidJobTransition(pair[0], pair[1]) {
			t.Errorf("expected %s -> %s to be invalid", pair[0], pair[1])
		}
	}
}

func TestRestartRequiresAnActiveServer(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.InstanceStatus{domain.InstanceRunning, domain.InstanceDegraded, domain.InstanceExternal} {
		if !restartableInstanceStatus(status) {
			t.Fatalf("active status %s cannot restart", status)
		}
	}
	for _, status := range []domain.InstanceStatus{domain.InstanceInstalling, domain.InstanceStopped, domain.InstanceStarting, domain.InstanceStopping, domain.InstanceUpdating} {
		if restartableInstanceStatus(status) {
			t.Fatalf("inactive status %s can restart", status)
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
