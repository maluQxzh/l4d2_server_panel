package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestSQLiteMigrationsAndCredentialLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sqliteVersion, err := store.SQLiteVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sqliteVersion == "" {
		t.Fatal("SQLite version is empty")
	}
	if err := store.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
	migrationsDir := filepath.Join("..", "..", "migrations", "sqlite")
	if err := store.Migrate(ctx, migrationsDir); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, migrationsDir); err != nil {
		t.Fatalf("migrations are not idempotent: %v", err)
	}
	setupKey, err := store.IssueSetupKey(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const ownerPassword = "integration-password-123"
	passwordHash, err := HashPassword(ownerPassword)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrapLogs strings.Builder
	bootstrapServer := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef"), CookieSecure: false,
		SessionTTL: time.Hour, Timezone: "UTC"}, store, slog.New(slog.NewTextHandler(&bootstrapLogs, nil)))
	defer bootstrapServer.Close()
	httpServer := httptest.NewServer(bootstrapServer.Handler())
	defer httpServer.Close()
	statusResponse, err := http.Get(httpServer.URL + "/api/v1/auth/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrapStatus struct {
		Required      bool   `json:"required"`
		KeyConfigured bool   `json:"key_configured"`
		CSRFToken     string `json:"csrf_token"`
	}
	if err := json.NewDecoder(statusResponse.Body).Decode(&bootstrapStatus); err != nil {
		statusResponse.Body.Close()
		t.Fatal(err)
	}
	statusResponse.Body.Close()
	if statusResponse.StatusCode != http.StatusOK || !bootstrapStatus.Required || !bootstrapStatus.KeyConfigured || bootstrapStatus.CSRFToken == "" || len(statusResponse.Cookies()) != 1 {
		t.Fatalf("unexpected bootstrap status: code=%d body=%#v cookies=%d", statusResponse.StatusCode, bootstrapStatus, len(statusResponse.Cookies()))
	}
	wrongBody := `{"setup_key":"` + setupKeyPrefix + strings.Repeat("A", 43) + `"}`
	wrongRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/api/v1/auth/bootstrap", strings.NewReader(wrongBody))
	if err != nil {
		t.Fatal(err)
	}
	wrongRequest.Header.Set("Content-Type", "application/json")
	wrongRequest.Header.Set("X-Bootstrap-CSRF-Token", bootstrapStatus.CSRFToken)
	wrongRequest.Header.Set("Idempotency-Key", "integration-bootstrap-wrong")
	wrongRequest.AddCookie(statusResponse.Cookies()[0])
	wrongResponse, err := http.DefaultClient.Do(wrongRequest)
	if err != nil {
		t.Fatal(err)
	}
	var wrongResult struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(wrongResponse.Body).Decode(&wrongResult); err != nil {
		wrongResponse.Body.Close()
		t.Fatal(err)
	}
	wrongResponse.Body.Close()
	if wrongResponse.StatusCode != http.StatusUnauthorized || wrongResult.Error.Code != "invalid_setup_key" {
		t.Fatalf("wrong setup key returned code=%d body=%#v", wrongResponse.StatusCode, wrongResult)
	}
	body := `{"setup_key":"` + setupKey + `"}`
	bootstrapRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/api/v1/auth/bootstrap", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	bootstrapRequest.Header.Set("Content-Type", "application/json")
	bootstrapRequest.Header.Set("X-Bootstrap-CSRF-Token", bootstrapStatus.CSRFToken)
	bootstrapRequest.Header.Set("Idempotency-Key", "integration-bootstrap")
	bootstrapRequest.AddCookie(statusResponse.Cookies()[0])
	bootstrapResponse, err := http.DefaultClient.Do(bootstrapRequest)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrapResult struct {
		User      domain.User `json:"user"`
		CSRFToken string      `json:"csrf_token"`
	}
	if err := json.NewDecoder(bootstrapResponse.Body).Decode(&bootstrapResult); err != nil {
		bootstrapResponse.Body.Close()
		t.Fatal(err)
	}
	bootstrapResponse.Body.Close()
	if bootstrapResponse.StatusCode != http.StatusCreated || bootstrapResult.User.Username != "admin" || bootstrapResult.User.Role != domain.RoleOwner || bootstrapResult.CSRFToken == "" {
		t.Fatalf("web bootstrap failed: code=%d response=%#v", bootstrapResponse.StatusCode, bootstrapResult)
	}
	owner := bootstrapResult.User
	loginRecord, err := store.UserForLogin(ctx, "admin")
	if err != nil || !VerifyPassword(loginRecord.PasswordHash, setupKey) {
		t.Fatalf("setup key was not installed as the initial owner password: %v", err)
	}
	if configured, err := store.SetupKeyConfigured(ctx); err != nil || configured {
		t.Fatalf("setup key was not consumed: configured=%v err=%v", configured, err)
	}
	auditItems, err := store.ListAudit(ctx, owner.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	auditJSON, err := json.Marshal(auditItems)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bootstrapLogs.String(), setupKey) || strings.Contains(string(auditJSON), setupKey) {
		t.Fatal("setup key leaked into bootstrap logs or audit output")
	}
	var created bool
	if _, created, err = store.BootstrapOwner(ctx, "other", "Other", passwordHash); err != nil || created {
		t.Fatalf("second bootstrap was not rejected: created=%v err=%v", created, err)
	}

	sessionSecret := "session-secret"
	csrfSecret := "csrf-secret"
	if _, err := store.CreateSession(ctx, owner.ID, tokenHash(sessionSecret), tokenHash(csrfSecret), "127.0.0.1", "integration", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	principal, err := store.Session(ctx, tokenHash(sessionSecret))
	if err != nil || !principal.GlobalAllows(PermissionRootSecurity) {
		t.Fatalf("owner session was not restored with bindings: %#v err=%v", principal, err)
	}

	adminUser, _, err := store.CreateTeamUser(ctx, owner.ID, CreateTeamUserInput{
		Username: "integration-admin", DisplayName: "Integration Admin",
		Bindings: []domain.RoleBinding{{Role: domain.RoleAdmin, ScopeType: "global"}},
	})
	if err != nil {
		t.Fatalf("owner could not create administrator: %v", err)
	}
	if _, _, err := store.CreateTeamUser(ctx, adminUser.ID, CreateTeamUserInput{
		Username: "viewer", DisplayName: "Viewer",
		Bindings: []domain.RoleBinding{{Role: domain.RoleViewer, ScopeType: "global"}},
	}); !errors.Is(err, errRootSecurityRequired) {
		t.Fatalf("administrator bypassed account creation boundary: %v", err)
	}
	changedName := "Unauthorized change"
	if _, err := store.UpdateTeamUser(ctx, adminUser.ID, owner.ID, UpdateTeamUserInput{DisplayName: &changedName}); !errors.Is(err, errRootSecurityRequired) {
		t.Fatalf("administrator bypassed account update boundary: %v", err)
	}

	rawToken, err := newAPITokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	token, err := store.CreateAPIToken(ctx, APIToken{UserID: owner.ID, Name: "integration", Scopes: []string{"read"}, ExpiresAt: &expires}, tokenHash(rawToken))
	if err != nil {
		t.Fatal(err)
	}
	tokenPrincipal, err := store.APITokenPrincipal(ctx, tokenHash(rawToken))
	if err != nil || !tokenPrincipal.Allows(PermissionRead) || tokenPrincipal.Allows(PermissionOperate) {
		t.Fatalf("API token scope was not enforced: %#v err=%v", tokenPrincipal, err)
	}
	if err := store.RevokeAPIToken(ctx, owner.ID, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.APITokenPrincipal(ctx, tokenHash(rawToken)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked API token remained usable: %v", err)
	}

	testReadAndWriteJobLocks(t, ctx, store, owner.ID)
	testInstancePortReservations(t, ctx, store)
	testPublicInstanceProjection(t, ctx, store)
	testBackupUploadLifecycle(t, ctx, store, owner.ID)
	testBackupUploadAbort(t, ctx, store, owner.ID)
	testBackupUploadAbortHandlerReplay(t, ctx, store, owner.ID)
	testConfirmedQuarantinePurge(t, ctx, store, owner.ID)
	testIdempotencyRequestEquivalence(t, ctx, store, owner.ID)
	testLateCancelTerminalOutcome(t, ctx, store, owner.ID)
	testAtomicCancellationProjection(t, ctx, store, owner.ID)
	testTerminalProjectionBarrier(t, ctx, store, owner.ID)
	testCancelDispatchFairness(t, ctx, store, owner.ID)
	testAutomaticMaintenanceBatch(t, ctx, store, owner.ID)
	testMaintenanceProbeInterlock(t, ctx, store)
	testResourceTelemetryProjection(t, ctx, store)
	testHeartbeatPreservesActiveWriteProjection(t, ctx, store, owner.ID)
}

func testHeartbeatPreservesActiveWriteProjection(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'heartbeat-projection','offline')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "heartbeat-projection", Port: 29325,
		Config: domain.InstanceConfig{Hostname: "heartbeat-projection", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}

	updateJob, _, err := store.QueueInstanceJob(ctx, instance.ID, "update", map[string]any{}, ownerID, "heartbeat-update")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, nodeID, "heartbeat-test-agent", map[string]string{instance.ID: "running"}); err != nil {
		t.Fatal(err)
	}
	protected, err := store.GetInstance(ctx, instance.ID)
	if err != nil || protected.Status != domain.InstanceUpdating {
		t.Fatalf("heartbeat overwrote active update projection: %#v err=%v", protected, err)
	}
	node, err := store.GetNode(ctx, nodeID)
	if err != nil || node.Status != domain.NodeOnline || node.AgentVersion != "heartbeat-test-agent" || node.LastSeenAt == nil {
		t.Fatalf("heartbeat did not refresh node health while preserving the instance projection: %#v err=%v", node, err)
	}

	terminal, err := store.UpdateJob(ctx, updateJob.ID, domain.JobSucceeded, 100, "updated", map[string]any{"build_id": "heartbeat-build"})
	if err != nil || !jobProjectionPending(terminal) {
		t.Fatalf("persist update terminal projection: %#v err=%v", terminal, err)
	}
	if _, applied, err := store.ApplyPendingJobProjection(ctx, terminal.ID); err != nil || !applied {
		t.Fatalf("apply update terminal projection: applied=%v err=%v", applied, err)
	}
	if err := store.Heartbeat(ctx, nodeID, "heartbeat-test-agent", map[string]string{instance.ID: "running"}); err != nil {
		t.Fatal(err)
	}
	stable, err := store.GetInstance(ctx, instance.ID)
	if err != nil || stable.Status != domain.InstanceRunning {
		t.Fatalf("heartbeat did not resume status projection after the write barrier cleared: %#v err=%v", stable, err)
	}

	// Configure has no dedicated enum value. Preserve whichever lifecycle state
	// Control published while the generic write lock is active, then resume
	// Agent state once the terminal projection marker has been cleared.
	configureJob, _, err := store.QueueInstanceJob(ctx, instance.ID, "configure", map[string]any{}, ownerID, "heartbeat-configure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='starting' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, nodeID, "heartbeat-test-agent", map[string]string{instance.ID: "stopped"}); err != nil {
		t.Fatal(err)
	}
	protected, err = store.GetInstance(ctx, instance.ID)
	if err != nil || protected.Status != domain.InstanceStarting {
		t.Fatalf("heartbeat overwrote an active configuration lifecycle state: %#v err=%v", protected, err)
	}
	terminal, err = store.UpdateJob(ctx, configureJob.ID, domain.JobSucceeded, 100, "configured", map[string]any{})
	if err != nil || !jobProjectionPending(terminal) {
		t.Fatalf("persist configure terminal projection: %#v err=%v", terminal, err)
	}
	if _, applied, err := store.ApplyPendingJobProjection(ctx, terminal.ID); err != nil || !applied {
		t.Fatalf("apply configure terminal projection: applied=%v err=%v", applied, err)
	}
	if err := store.Heartbeat(ctx, nodeID, "heartbeat-test-agent", map[string]string{instance.ID: "stopped"}); err != nil {
		t.Fatal(err)
	}
	stable, err = store.GetInstance(ctx, instance.ID)
	if err != nil || stable.Status != domain.InstanceStopped {
		t.Fatalf("heartbeat remained blocked after configure projection completed: %#v err=%v", stable, err)
	}

	// Reproduce the queue/heartbeat race: the job transaction owns the same
	// instance row while its insert is not visible to the heartbeat statement.
	// Heartbeat must wait, then take a fresh snapshot before applying status.
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='starting' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	queueTx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer queueTx.Rollback()
	raceJobID := uuid.NewString()
	if _, err := queueTx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,requested_by)
		VALUES(?1,?2,?3,'backup','queued','{}',?4)`, raceJobID, nodeID, instance.ID, ownerID); err != nil {
		t.Fatal(err)
	}
	heartbeatStarted := make(chan struct{})
	heartbeatDone := make(chan error, 1)
	go func() {
		close(heartbeatStarted)
		heartbeatDone <- store.Heartbeat(ctx, nodeID, "heartbeat-race-agent", map[string]string{instance.ID: "stopped"})
	}()
	<-heartbeatStarted
	select {
	case err := <-heartbeatDone:
		t.Fatalf("heartbeat bypassed the active SQLite write transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := queueTx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-heartbeatDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("heartbeat did not resume after the concurrent job transaction committed")
	}
	protected, err = store.GetInstance(ctx, instance.ID)
	if err != nil || protected.Status != domain.InstanceStarting {
		t.Fatalf("concurrent job publication raced past heartbeat protection: %#v err=%v", protected, err)
	}
	if _, _, err := store.CancelJob(ctx, raceJobID); err != nil {
		t.Fatalf("clean up heartbeat race job: %v", err)
	}
	if err := store.Heartbeat(ctx, nodeID, "heartbeat-race-agent", map[string]string{instance.ID: "stopped"}); err != nil {
		t.Fatal(err)
	}

	// Read probes deliberately do not own the write barrier.
	readJob, _, err := store.QueueInstanceJob(ctx, instance.ID, "a2s.info", map[string]any{}, ownerID, "heartbeat-read")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(ctx, nodeID, "heartbeat-test-agent", map[string]string{instance.ID: "running"}); err != nil {
		t.Fatal(err)
	}
	stable, err = store.GetInstance(ctx, instance.ID)
	if err != nil || stable.Status != domain.InstanceRunning {
		t.Fatalf("read-only probe incorrectly blocked heartbeat state: %#v err=%v", stable, err)
	}
	if _, _, err := store.CancelJob(ctx, readJob.ID); err != nil {
		t.Fatalf("clean up heartbeat read probe: %v", err)
	}
}

func testResourceTelemetryProjection(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'telemetry-projection','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMetric(ctx, domain.MetricSample{NodeID: nodeID, CPUPercent: 12.5, MemoryUsed: 100, MemoryTotal: 200,
		DiskUsed: 300, DiskTotal: 400, NetworkRx: 5_000, NetworkTx: 2_000, CollectedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	node, err := store.GetNode(ctx, nodeID)
	if err != nil || node.NetworkRx != 5_000 || node.NetworkTx != 2_000 {
		t.Fatalf("latest node network projection: %#v err=%v", node, err)
	}
	nodes, err := store.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundNode := false
	for _, candidate := range nodes {
		if candidate.ID == nodeID {
			foundNode = candidate.NetworkRx == 5_000 && candidate.NetworkTx == 2_000
		}
	}
	if !foundNode {
		t.Fatalf("node list omitted latest network projection: %#v", nodes)
	}

	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "telemetry-instance", Port: 29315,
		Config: domain.InstanceConfig{Hostname: "telemetry-instance", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO instance_update_state(instance_id,installed_build,available_build,available,checked_at)
		VALUES(?1,'123456','654321',true,?2)`, instance.ID, checkedAt); err != nil {
		t.Fatal(err)
	}
	projected, err := store.GetInstance(ctx, instance.ID)
	if err != nil || !projected.UpdateAvailable || projected.AvailableBuild != "654321" || projected.UpdateCheckedAt == nil || !projected.UpdateCheckedAt.Equal(checkedAt) {
		t.Fatalf("instance update projection: %#v err=%v", projected, err)
	}
	if err := store.ApplyJobResult(ctx, domain.Job{InstanceID: instance.ID, Type: "validate", Status: domain.JobSucceeded,
		Result: map[string]any{"build_id": "654321"}}); err != nil {
		t.Fatal(err)
	}
	projected, err = store.GetInstance(ctx, instance.ID)
	if err != nil || !projected.UpdateAvailable {
		t.Fatalf("validation incorrectly cleared update availability: %#v err=%v", projected, err)
	}
	if err := store.ApplyJobResult(ctx, domain.Job{InstanceID: instance.ID, Type: "update", Status: domain.JobSucceeded,
		Result: map[string]any{"build_id": "654321"}}); err != nil {
		t.Fatal(err)
	}
	projected, err = store.GetInstance(ctx, instance.ID)
	if err != nil || projected.UpdateAvailable || projected.AvailableBuild != "654321" || projected.UpdateCheckedAt == nil || !projected.UpdateCheckedAt.After(checkedAt) {
		t.Fatalf("successful update did not clear availability: %#v err=%v", projected, err)
	}
	var installedBuild string
	if err := store.db.QueryRowContext(ctx, `SELECT installed_build FROM instance_update_state WHERE instance_id=?1`, instance.ID).Scan(&installedBuild); err != nil || installedBuild != "654321" {
		t.Fatalf("successful update installed build = %q err=%v", installedBuild, err)
	}
}

func testTerminalProjectionBarrier(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'terminal-projection','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "terminal-projection", Port: 29215,
		Config: domain.InstanceConfig{Hostname: "terminal-projection", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "update", map[string]any{}, ownerID, "terminal-projection-success")
	if err != nil {
		t.Fatal(err)
	}
	if marked, markErr := store.MarkDispatched(ctx, job.ID); markErr != nil || !marked {
		t.Fatalf("dispatch projection candidate: marked=%v err=%v", marked, markErr)
	}
	result := map[string]any{"build_id": "projection-build", "was_running": false}
	terminal, err := store.UpdateJob(ctx, job.ID, domain.JobSucceeded, 100, "update completed", result)
	if err != nil || !jobProjectionPending(terminal) {
		t.Fatalf("persist terminal projection outbox: %#v err=%v", terminal, err)
	}
	crashSnapshot, err := store.GetInstance(ctx, instance.ID)
	if err != nil || crashSnapshot.Status != domain.InstanceUpdating {
		t.Fatalf("pre-projection crash snapshot was not preserved: %#v err=%v", crashSnapshot, err)
	}
	if _, _, queueErr := store.QueueInstanceJob(ctx, instance.ID, "validate", map[string]any{}, ownerID, "terminal-projection-blocked"); !errors.Is(queueErr, ErrConflict) {
		t.Fatalf("terminal projection outbox released its write lock early: %v", queueErr)
	}
	if removeErr := store.RemoveInstanceRecord(ctx, instance.ID); !errors.Is(removeErr, ErrConflict) {
		t.Fatalf("terminal projection outbox allowed instance deletion: %v", removeErr)
	}

	// Force the first projection attempt to fail after the terminal result is
	// durable. The marker and unique write lock must survive for journal replay.
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_terminal_projection_test
		BEFORE UPDATE ON instances BEGIN
			SELECT RAISE(ABORT,'injected terminal projection failure');
		END`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fail_terminal_projection_test`)
	}()
	if _, _, projectionErr := store.ApplyPendingJobProjection(ctx, terminal.ID); projectionErr == nil {
		t.Fatal("injected terminal projection failure was ignored")
	}
	pending, err := store.GetJob(ctx, terminal.ID)
	if err != nil || !jobProjectionPending(pending) {
		t.Fatalf("failed projection lost its durable marker: %#v err=%v", pending, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_terminal_projection_test`); err != nil {
		t.Fatal(err)
	}

	projected, applied, err := store.ApplyPendingJobProjection(ctx, terminal.ID)
	if err != nil || !applied || jobProjectionPending(projected) {
		t.Fatalf("replay terminal projection: %#v applied=%v err=%v", projected, applied, err)
	}
	projectedInstance, err := store.GetInstance(ctx, instance.ID)
	if err != nil || projectedInstance.Status != domain.InstanceStopped || projectedInstance.BuildID != "projection-build" {
		t.Fatalf("terminal projection result: %#v err=%v", projectedInstance, err)
	}

	newWork, _, err := store.QueueInstanceJob(ctx, instance.ID, "start", map[string]any{}, ownerID, "terminal-projection-new-work")
	if err != nil {
		t.Fatalf("projection did not release write lock: %v", err)
	}
	if replay, replayErr := store.UpdateJob(ctx, terminal.ID, domain.JobSucceeded, 100, "update completed", result); replayErr != nil ||
		replay.ID != terminal.ID || jobProjectionPending(replay) {
		t.Fatalf("terminal ACK replay changed immutable outcome: %#v err=%v", replay, replayErr)
	}
	if replay, replayApplied, replayErr := store.ApplyPendingJobProjection(ctx, terminal.ID); replayErr != nil || replayApplied || replay.ID != terminal.ID {
		t.Fatalf("cleared terminal projection replayed side effects: %#v applied=%v err=%v", replay, replayApplied, replayErr)
	}
	projectedInstance, err = store.GetInstance(ctx, instance.ID)
	if err != nil || projectedInstance.Status != domain.InstanceStarting {
		t.Fatalf("old terminal projection overwrote newer work: %#v err=%v", projectedInstance, err)
	}
	if _, _, err := store.CancelJob(ctx, newWork.ID); err != nil {
		t.Fatalf("clean up projection replay work: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	failedJob, _, err := store.QueueInstanceJob(ctx, instance.ID, "update", map[string]any{}, ownerID, "terminal-projection-failure")
	if err != nil {
		t.Fatal(err)
	}
	if marked, markErr := store.MarkDispatched(ctx, failedJob.ID); markErr != nil || !marked {
		t.Fatalf("dispatch failed projection candidate: marked=%v err=%v", marked, markErr)
	}
	failed, err := store.UpdateJob(ctx, failedJob.ID, domain.JobFailed, 55, "SteamCMD unavailable", map[string]any{})
	if err != nil || !jobProjectionPending(failed) {
		t.Fatalf("persist failed projection outbox: %#v err=%v", failed, err)
	}
	failed, applied, err = store.ApplyPendingJobProjection(ctx, failed.ID)
	if err != nil || !applied || jobProjectionPending(failed) {
		t.Fatalf("project failed terminal job: %#v applied=%v err=%v", failed, applied, err)
	}
	projectedInstance, err = store.GetInstance(ctx, instance.ID)
	if err != nil || projectedInstance.Status != domain.InstanceDegraded {
		t.Fatalf("failed write did not leave a stable instance state: %#v err=%v", projectedInstance, err)
	}

	removalTarget, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "terminal-projection-delete", Port: 29216,
		Config: domain.InstanceConfig{Hostname: "terminal-projection-delete", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	removeJob, _, err := store.QueueInstanceJob(ctx, removalTarget.ID, "delete", map[string]any{"mode": "quarantine"}, ownerID,
		"terminal-projection-delete")
	if err != nil {
		t.Fatal(err)
	}
	if marked, markErr := store.MarkDispatched(ctx, removeJob.ID); markErr != nil || !marked {
		t.Fatalf("dispatch delete projection candidate: marked=%v err=%v", marked, markErr)
	}
	entryName := removalTarget.ID + "-" + time.Now().UTC().Format("20060102T150405Z")
	removeTerminal, err := store.UpdateJob(ctx, removeJob.ID, domain.JobSucceeded, 100, "instance quarantined",
		map[string]any{"quarantine_entry": entryName, "root_index": 0})
	if err != nil || !jobProjectionPending(removeTerminal) {
		t.Fatalf("persist delete projection outbox: %#v err=%v", removeTerminal, err)
	}
	removed, applied, err := store.ApplyPendingJobProjection(ctx, removeTerminal.ID)
	if err != nil || !applied || jobProjectionPending(removed) || removed.InstanceID != "" {
		t.Fatalf("project instance deletion: %#v applied=%v err=%v", removed, applied, err)
	}
	if _, err := store.GetInstance(ctx, removalTarget.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("projected instance deletion remained visible: %v", err)
	}
	if stringFromAny(removed.Result["quarantine_id"]) == "" {
		t.Fatalf("delete projection result enrichment was lost while clearing marker: %#v", removed.Result)
	}
}

func testBackupUploadAbortHandlerReplay(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'upload-abort-http-node','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "upload-abort-http-target", Port: 29118,
		Config: domain.InstanceConfig{Hostname: "abort-http-target", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	upload, _, err := store.CreateBackupUpload(ctx, instance.ID, "http-abandon.zip", "linux/amd64", 8,
		strings.Repeat("c", 64), ownerID, "create-http-abandon-upload")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer server.Close()
	invoke := func() *httptest.ResponseRecorder {
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", instance.ID)
		route.URLParams.Add("uploadID", upload.ID)
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/instances/"+instance.ID+"/backup-uploads/"+upload.ID, nil)
		request.Header.Set("Idempotency-Key", "http-abort-key")
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
		request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
			User: domain.User{ID: ownerID, Username: "owner", Role: domain.RoleOwner},
		}))
		response := httptest.NewRecorder()
		server.handleAbortBackupUpload(response, request)
		return response
	}
	first := invoke()
	second := invoke()
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("abort handler responses: first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	job, err := scanJob(store.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, ownerID, "http-abort-key"))
	if err != nil || job.Type != "backup.upload.abort" || !strings.Contains(first.Body.String(), job.ID) || !strings.Contains(second.Body.String(), job.ID) {
		t.Fatalf("abort handler did not replay original job: job=%#v first=%s second=%s err=%v", job, first.Body.String(), second.Body.String(), err)
	}
	var eventCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM event_log WHERE type='job.queued' AND resource_id=?1`, job.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("idempotent abort published %d queued events, want 1", eventCount)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT json_extract(metadata,'$.idempotent_replay') FROM audit_events
		WHERE action='instance.backup_upload.abort_requested' AND json_extract(metadata,'$.upload_id')=?1 ORDER BY created_at,id`, upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var replayFlags []bool
	for rows.Next() {
		var replay bool
		if err := rows.Scan(&replay); err != nil {
			t.Fatal(err)
		}
		replayFlags = append(replayFlags, replay)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(replayFlags) != 2 || replayFlags[0] || !replayFlags[1] {
		t.Fatalf("abort replay audit flags = %#v", replayFlags)
	}
}

func testAtomicCancellationProjection(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'atomic-cancel','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "atomic-cancel", Port: 29214,
		Config: domain.InstanceConfig{Hostname: "atomic-cancel", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}

	actions := []string{"start", "stop", "restart", "install", "reinstall", "import", "configure", "update", "validate"}
	var lastCanceled domain.Job
	for index, action := range actions {
		status := domain.InstanceStopped
		if action == "restart" {
			status = domain.InstanceRunning
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status=?2 WHERE id=?1`, instance.ID, status); err != nil {
			t.Fatal(err)
		}
		job, reused, err := store.QueueInstanceJob(ctx, instance.ID, action, map[string]any{}, ownerID,
			fmt.Sprintf("atomic-cancel-%s", action))
		if err != nil || reused {
			t.Fatalf("queue %s cancellation candidate: reused=%v err=%v", action, reused, err)
		}

		// The first case races several HTTP-equivalent cancellation requests.
		// They must serialize in instance -> job order, settle only once, and
		// all observe a valid idempotent result.
		if index == 0 {
			const contenders = 8
			start := make(chan struct{})
			results := make(chan error, contenders)
			for contender := 0; contender < contenders; contender++ {
				go func() {
					<-start
					canceled, previous, cancelErr := store.CancelJob(ctx, job.ID)
					if cancelErr != nil {
						results <- cancelErr
						return
					}
					if canceled.Status != domain.JobCanceled || (previous != domain.JobQueued && previous != domain.JobCanceled) {
						results <- fmt.Errorf("unexpected concurrent cancellation: status=%s previous=%s", canceled.Status, previous)
						return
					}
					results <- nil
				}()
			}
			close(start)
			for contender := 0; contender < contenders; contender++ {
				if cancelErr := <-results; cancelErr != nil {
					t.Fatalf("concurrent cancellation %d: %v", contender, cancelErr)
				}
			}
			lastCanceled, err = store.GetJob(ctx, job.ID)
		} else {
			var previous domain.JobStatus
			lastCanceled, previous, err = store.CancelJob(ctx, job.ID)
			if previous != domain.JobQueued {
				t.Fatalf("%s cancellation previous status = %s, want queued", action, previous)
			}
		}
		if err != nil || lastCanceled.Status != domain.JobCanceled {
			t.Fatalf("cancel queued %s: %#v err=%v", action, lastCanceled, err)
		}
		projected, projectionErr := store.GetInstance(ctx, instance.ID)
		if projectionErr != nil || projected.Status != domain.InstanceDegraded {
			t.Fatalf("queued %s cancellation was visible before its projection: %#v err=%v", action, projected, projectionErr)
		}
	}

	// A retry of an old cancellation after newer work is queued is a no-op. A
	// post-commit projection would incorrectly turn this new "starting" state
	// back into "degraded".
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	newJob, _, err := store.QueueInstanceJob(ctx, instance.ID, "start", map[string]any{}, ownerID, "atomic-cancel-new-work")
	if err != nil {
		t.Fatal(err)
	}
	if replayed, previous, replayErr := store.CancelJob(ctx, lastCanceled.ID); replayErr != nil || previous != domain.JobCanceled || replayed.ID != lastCanceled.ID {
		t.Fatalf("replay old cancellation: %#v previous=%s err=%v", replayed, previous, replayErr)
	}
	projected, err := store.GetInstance(ctx, instance.ID)
	if err != nil || projected.Status != domain.InstanceStarting {
		t.Fatalf("old cancellation overwrote newer work: %#v err=%v", projected, err)
	}
	if _, _, err := store.CancelJob(ctx, newJob.ID); err != nil {
		t.Fatalf("clean up newer work: %v", err)
	}
}

func testBackupUploadAbort(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'upload-abort-node','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "upload-abort-target", Port: 29117,
		Config: domain.InstanceConfig{Hostname: "abort-target", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	fileSHA := strings.Repeat("a", 64)
	upload, reused, err := store.CreateBackupUpload(ctx, instance.ID, "abandon.zip", "linux/amd64", 8, fileSHA, ownerID, "create-abandon-upload")
	if err != nil || reused {
		t.Fatalf("create abortable upload: reused=%v err=%v", reused, err)
	}
	chunkPayload := map[string]any{"upload_id": upload.ID, "offset": int64(0), "length": 4,
		"chunk_sha256": strings.Repeat("b", 64), "expected_size": 8, "expected_sha256": fileSHA,
		"content_base64": encryptedValuePlaceholder, "content_base64_ciphertext": "encrypted-abort-body"}
	chunk, reused, err := store.QueueBackupUploadChunk(ctx, instance.ID, upload.ID, chunkPayload, ownerID, "abandon-active-chunk")
	if err != nil || reused {
		t.Fatalf("queue chunk before abandon: reused=%v err=%v", reused, err)
	}
	if _, _, err := store.QueueBackupUploadAbort(ctx, instance.ID, upload.ID, ownerID, "abort-upload"); !errors.Is(err, ErrConflict) {
		t.Fatalf("active chunk did not block upload abandonment: %v", err)
	}
	if marked, err := store.MarkDispatched(ctx, chunk.ID); err != nil || !marked {
		t.Fatalf("dispatch abort-test chunk: marked=%v err=%v", marked, err)
	}
	completed, err := store.UpdateJob(ctx, chunk.ID, domain.JobSucceeded, 100, "uploaded", map[string]any{
		"upload_id": upload.ID, "offset": int64(0), "length": 4, "received_size": int64(4),
		"chunk_sha256": strings.Repeat("b", 64), "complete": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueBackupUploadAbort(ctx, instance.ID, upload.ID, ownerID, "abort-upload"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unprojected successful chunk did not block upload abandonment: %v", err)
	}
	if projected, applied, projectionErr := store.ApplyPendingJobProjection(ctx, completed.ID); projectionErr != nil || !applied || jobProjectionPending(projected) {
		t.Fatalf("project abort-test chunk: %#v applied=%v err=%v", projected, applied, projectionErr)
	}
	abortJob, reused, err := store.QueueBackupUploadAbort(ctx, instance.ID, upload.ID, ownerID, "abort-upload")
	if err != nil || reused || abortJob.Type != "backup.upload.abort" || abortJob.InstanceID != instance.ID || abortJob.Payload["upload_id"] != upload.ID {
		t.Fatalf("queue upload abandonment: job=%#v reused=%v err=%v", abortJob, reused, err)
	}
	// The game-write index deliberately excludes staging cleanup, so an
	// unrelated instance operation may be queued concurrently.
	startJob, reused, err := store.QueueInstanceJob(ctx, instance.ID, "start", map[string]any{}, ownerID, "start-while-upload-abort")
	if err != nil || reused || startJob.ID == "" {
		t.Fatalf("upload abandonment blocked a game write: job=%#v reused=%v err=%v", startJob, reused, err)
	}
	abandoned, err := store.GetBackupUpload(ctx, instance.ID, upload.ID)
	if err != nil || abandoned.Status != "expired" {
		t.Fatalf("abandoned upload was not atomically expired: %#v err=%v", abandoned, err)
	}
	replay, reused, err := store.QueueBackupUploadAbort(ctx, instance.ID, upload.ID, ownerID, "abort-upload")
	if err != nil || !reused || replay.ID != abortJob.ID {
		t.Fatalf("upload abandonment replay: job=%#v reused=%v err=%v", replay, reused, err)
	}
	other, _, err := store.CreateBackupUpload(ctx, instance.ID, "different.zip", "linux/amd64", 8, fileSHA, ownerID, "create-other-abandon-upload")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueBackupUploadAbort(ctx, instance.ID, other.ID, ownerID, "abort-upload"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same idempotency key accepted a different upload: %v", err)
	}
}

func testCancelDispatchFairness(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	const dispatchLimit = 5
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'cancel-fairness','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instanceIDs := make([]string, 0, dispatchLimit)
	for index := 0; index < dispatchLimit; index++ {
		instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: fmt.Sprintf("cancel-fairness-%d", index),
			Port: 29300 + index, Config: domain.InstanceConfig{Hostname: "fairness", MaxPlayers: 8}})
		if err != nil {
			t.Fatal(err)
		}
		instanceIDs = append(instanceIDs, instance.ID)
		if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
			t.Fatal(err)
		}
		job, _, err := store.QueueInstanceJob(ctx, instance.ID, "update", map[string]any{}, ownerID, fmt.Sprintf("fairness-cancel-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		if marked, err := store.MarkDispatched(ctx, job.ID); err != nil || !marked {
			t.Fatalf("dispatch cancellation candidate: marked=%v err=%v", marked, err)
		}
		if canceled, previous, err := store.CancelJob(ctx, job.ID); err != nil || previous != domain.JobDispatched || canceled.Result["cancel_pending"] != true {
			t.Fatalf("create pending cancellation: %#v previous=%s err=%v", canceled, previous, err)
		}
	}
	ordinary, _, err := store.QueueInstanceJob(ctx, instanceIDs[0], "a2s.info", map[string]any{"probe": "fair"}, ownerID, "fairness-ordinary")
	if err != nil {
		t.Fatal(err)
	}
	dispatchable, err := store.JobsForDispatch(ctx, nodeID, dispatchLimit)
	if err != nil {
		t.Fatal(err)
	}
	foundOrdinary, pendingCancellations := false, 0
	for _, candidate := range dispatchable {
		if candidate.ID == ordinary.ID {
			foundOrdinary = true
		}
		if candidate.Status == domain.JobCanceled && candidate.Result["cancel_pending"] == true {
			pendingCancellations++
		}
	}
	if !foundOrdinary || pendingCancellations != dispatchLimit {
		t.Fatalf("dispatch quotas were not independent: ordinary=%v cancellations=%d jobs=%d", foundOrdinary, pendingCancellations, len(dispatchable))
	}
}

func testAutomaticMaintenanceBatch(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'automatic-maintenance','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	nextPort := 31000
	create := func(node, name string, status domain.InstanceStatus, maintenanceCron string, available bool) domain.Instance {
		t.Helper()
		instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: node, Name: name, Port: nextPort,
			Config: domain.InstanceConfig{Hostname: name, MaxPlayers: 8, MaintenanceCron: maintenanceCron}})
		nextPort++
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status=?2 WHERE id=?1`, instance.ID, status); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO instance_update_state(instance_id,installed_build,available_build,available,checked_at)
			VALUES(?1,'old-build','new-build',?2,?3)`, instance.ID, available, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		instance, err = store.GetInstance(ctx, instance.ID)
		if err != nil {
			t.Fatal(err)
		}
		return instance
	}

	cronExpression := "0 4 * * *"
	automatic := []domain.Instance{
		create(nodeID, "maintenance-running", domain.InstanceRunning, cronExpression, true),
		create(nodeID, "maintenance-stopped", domain.InstanceStopped, cronExpression, true),
		create(nodeID, "maintenance-degraded", domain.InstanceDegraded, cronExpression, true),
	}
	changedAfterSelection := create(nodeID, "maintenance-status-race", domain.InstanceRunning, cronExpression, true)
	unavailable := create(nodeID, "maintenance-unavailable", domain.InstanceStopped, cronExpression, false)
	withoutCron := create(nodeID, "maintenance-no-cron", domain.InstanceStopped, "", true)

	busyNodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'maintenance-busy','online')`, busyNodeID); err != nil {
		t.Fatal(err)
	}
	busy := create(busyNodeID, "maintenance-busy", domain.InstanceStopped, cronExpression, true)
	if _, _, err := store.QueueInstanceJob(ctx, busy.ID, "update", map[string]any{}, ownerID, "maintenance-busy-write"); err != nil {
		t.Fatal(err)
	}
	probeNodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'maintenance-probe','online')`, probeNodeID); err != nil {
		t.Fatal(err)
	}
	probeBusy := create(probeNodeID, "maintenance-probe", domain.InstanceStopped, cronExpression, true)
	if _, _, err := store.QueueSystemProbe(ctx, monitoringTarget{InstanceID: probeBusy.ID, NodeID: probeNodeID}, "check_update", "maintenance-active-probe"); err != nil {
		t.Fatal(err)
	}
	offlineNodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'maintenance-offline','offline')`, offlineNodeID); err != nil {
		t.Fatal(err)
	}
	offline := create(offlineNodeID, "maintenance-offline", domain.InstanceStopped, cronExpression, true)

	// Old ordinary work must retain its own dispatch quota even when a batch is
	// due, and conversely must not starve the single automatic candidate.
	manualInstance := create(nodeID, "maintenance-manual", domain.InstanceStopped, "", false)
	manualJob, _, err := store.QueueInstanceJob(ctx, manualInstance.ID, "a2s.info", map[string]any{"probe": "manual"}, ownerID, "maintenance-manual-job")
	if err != nil {
		t.Fatal(err)
	}
	scheduledInstance := create(nodeID, "maintenance-scheduled", domain.InstanceStopped, "", false)
	scheduledJob, _, err := store.QueueInstanceJob(ctx, scheduledInstance.ID, "rcon", map[string]any{"command": "status"}, "", "schedule:maintenance-fairness")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 21; index++ {
		ordinary := create(nodeID, fmt.Sprintf("maintenance-ordinary-%d", index), domain.InstanceStopped, "", false)
		if _, _, err := store.QueueInstanceJob(ctx, ordinary.ID, "a2s.info", map[string]any{"probe": index}, ownerID,
			fmt.Sprintf("maintenance-ordinary-job-%d", index)); err != nil {
			t.Fatal(err)
		}
	}

	candidates, err := store.AutomaticMaintenanceCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := make(map[string]maintenanceUpdateCandidate)
	for _, candidate := range candidates {
		selected[candidate.Instance.ID] = candidate
	}
	for _, instance := range append(append([]domain.Instance{}, automatic...), changedAfterSelection) {
		if _, ok := selected[instance.ID]; !ok {
			t.Fatalf("eligible maintenance instance %s was omitted", instance.ID)
		}
	}
	for _, instance := range []domain.Instance{unavailable, withoutCron, busy, probeBusy, offline} {
		if _, ok := selected[instance.ID]; ok {
			t.Fatalf("ineligible maintenance instance %s was selected", instance.ID)
		}
	}

	batchCandidates := make([]maintenanceUpdateCandidate, 0, len(automatic)+1)
	for _, instance := range automatic {
		batchCandidates = append(batchCandidates, selected[instance.ID])
	}
	batchCandidates = append(batchCandidates, selected[changedAfterSelection.ID])
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='starting' WHERE id=?1`, changedAfterSelection.ID); err != nil {
		t.Fatal(err)
	}
	window := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	batch, err := store.QueueAutomaticMaintenanceBatch(ctx, nodeID, window, batchCandidates)
	if err != nil || batch.Blocked || len(batch.Jobs) != len(automatic) {
		t.Fatalf("queue maintenance batch: blocked=%v jobs=%d err=%v", batch.Blocked, len(batch.Jobs), err)
	}
	originalStatus := make(map[string]domain.InstanceStatus, len(automatic))
	jobsByID := make(map[string]domain.Job, len(batch.Jobs))
	for _, instance := range automatic {
		originalStatus[instance.ID] = instance.Status
	}
	for _, item := range batch.Jobs {
		if item.Reused || !isAutomaticMaintenanceUpdate(item.Job) {
			t.Fatalf("new batch returned invalid job: %#v", item)
		}
		jobsByID[item.Job.ID] = item.Job
		persisted, err := store.GetInstance(ctx, item.Job.InstanceID)
		if err != nil || persisted.Status != originalStatus[item.Job.InstanceID] {
			t.Fatalf("queued job changed instance status: %#v err=%v", persisted, err)
		}
		snapshotStatus := domain.InstanceStatus("")
		switch snapshot := item.Job.Payload["instance"].(type) {
		case domain.Instance:
			snapshotStatus = snapshot.Status
		case map[string]any:
			snapshotStatus = domain.InstanceStatus(stringFromAny(snapshot["status"]))
		}
		if snapshotStatus != originalStatus[item.Job.InstanceID] {
			t.Fatalf("queued job lost pre-dispatch status snapshot: %#v", item.Job.Payload["instance"])
		}
	}

	replay, err := store.QueueAutomaticMaintenanceBatch(ctx, nodeID, window, batchCandidates)
	if err != nil || replay.Blocked || len(replay.Jobs) != len(automatic) {
		t.Fatalf("replay maintenance batch: blocked=%v jobs=%d err=%v", replay.Blocked, len(replay.Jobs), err)
	}
	for _, item := range replay.Jobs {
		if !item.Reused {
			t.Fatalf("same-window replay created a new job: %#v", item.Job)
		}
	}
	var jobCount, auditCount, notificationCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE node_id=?1 AND idempotency_key LIKE 'maintenance:update:%'`, nodeID).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action='maintenance.update.queued'
		AND json_extract(metadata,'$.node_id')=?1`, nodeID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE type='maintenance.update.queued'
		AND json_extract(metadata,'$.node_id')=?1`, nodeID).Scan(&notificationCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != len(automatic) || auditCount != len(automatic) || notificationCount != len(automatic) {
		t.Fatalf("maintenance idempotency records: jobs=%d audit=%d notifications=%d", jobCount, auditCount, notificationCount)
	}
	blocked, err := store.QueueAutomaticMaintenanceBatch(ctx, nodeID, window.Add(24*time.Hour), batchCandidates)
	if err != nil || !blocked.Blocked || len(blocked.Jobs) != 0 {
		t.Fatalf("active previous window did not block new batch: %#v err=%v", blocked, err)
	}

	checkJobs := make(map[string]domain.Job, len(automatic))
	for _, instance := range automatic {
		if _, _, err := store.QueueSystemProbe(ctx, monitoringTarget{InstanceID: instance.ID, NodeID: nodeID}, "check_update",
			"maintenance-post-queue-check:"+instance.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("update probe entered after automatic batch: %v", err)
		}
		// Preserve one legacy/interleaving-shaped queued row to verify the
		// dispatch-time defense independently of queue-time serialization.
		check := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instance.ID, Type: "check_update",
			Status: domain.JobQueued, Payload: map[string]any{}, IdempotencyKey: "maintenance-legacy-check:" + instance.ID, CreatedAt: time.Now().UTC()}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,created_at)
			VALUES(?1,?2,?3,'check_update','queued','{}',?4,?5)`, check.ID, check.NodeID, check.InstanceID,
			check.IdempotencyKey, check.CreatedAt); err != nil {
			t.Fatal(err)
		}
		checkJobs[instance.ID] = check
	}
	dispatchable, err := store.JobsForDispatch(ctx, nodeID, 5)
	if err != nil {
		t.Fatal(err)
	}
	automaticJobs, ordinaryJobs := make([]domain.Job, 0, 1), 0
	seenManual, seenScheduled := false, false
	for _, job := range dispatchable {
		if isAutomaticMaintenanceUpdate(job) {
			automaticJobs = append(automaticJobs, job)
		} else if job.Status != domain.JobCanceled {
			ordinaryJobs++
		}
		seenManual = seenManual || job.ID == manualJob.ID
		seenScheduled = seenScheduled || job.ID == scheduledJob.ID
		for _, check := range checkJobs {
			if job.ID == check.ID {
				t.Fatalf("check_update dispatched while its instance update was active: %#v", job)
			}
		}
	}
	if len(automaticJobs) != 1 || ordinaryJobs != 5 || !seenManual || !seenScheduled {
		t.Fatalf("independent dispatch quotas failed: auto=%d ordinary=%d manual=%v scheduled=%v jobs=%d",
			len(automaticJobs), ordinaryJobs, seenManual, seenScheduled, len(dispatchable))
	}
	first := automaticJobs[0]
	if marked, err := store.MarkDispatched(ctx, first.ID); err != nil || !marked {
		t.Fatalf("claim automatic update: marked=%v err=%v", marked, err)
	}
	claimedInstance, err := store.GetInstance(ctx, first.InstanceID)
	if err != nil || claimedInstance.Status != domain.InstanceUpdating {
		t.Fatalf("dispatch did not publish updating atomically: %#v err=%v", claimedInstance, err)
	}
	for instanceID, status := range originalStatus {
		if instanceID == first.InstanceID {
			continue
		}
		instance, err := store.GetInstance(ctx, instanceID)
		if err != nil || instance.Status != status {
			t.Fatalf("dispatch changed a queued peer: %#v err=%v", instance, err)
		}
	}
	if _, err := store.UpdateJob(ctx, first.ID, domain.JobRunning, 25, "maintenance running", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"build_id": "maintenance-build", "was_running": originalStatus[first.InstanceID] == domain.InstanceRunning}
	if originalStatus[first.InstanceID] == domain.InstanceRunning {
		result["restart"] = map[string]any{"pid": 3210, "started_at": time.Now().UTC().Format(time.RFC3339Nano)}
	}
	terminal, err := store.UpdateJob(ctx, first.ID, domain.JobSucceeded, 100, "maintenance complete", result)
	if err != nil || !jobProjectionPending(terminal) {
		t.Fatalf("persist maintenance terminal projection: %#v err=%v", terminal, err)
	}
	beforeProjection, err := store.JobsForDispatch(ctx, nodeID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range beforeProjection {
		if isAutomaticMaintenanceUpdate(job) {
			t.Fatalf("next automatic job escaped terminal projection barrier: %#v", job)
		}
	}
	projected, applied, err := store.ApplyPendingJobProjection(ctx, first.ID)
	if err != nil || !applied || jobProjectionPending(projected) {
		t.Fatalf("apply maintenance projection: applied=%v job=%#v err=%v", applied, projected, err)
	}
	afterProjection, err := store.JobsForDispatch(ctx, nodeID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var nextAutomatic domain.Job
	foundReleasedCheck := false
	for _, job := range afterProjection {
		if isAutomaticMaintenanceUpdate(job) {
			nextAutomatic = job
		}
		foundReleasedCheck = foundReleasedCheck || job.ID == checkJobs[first.InstanceID].ID
	}
	if nextAutomatic.ID == "" || !foundReleasedCheck {
		t.Fatalf("projection did not release next update/check: next=%#v check=%v", nextAutomatic, foundReleasedCheck)
	}

	// A queued cancellation never published updating and restores the exact
	// pre-dispatch snapshot rather than degrading a still-healthy instance.
	beforeCancel := originalStatus[nextAutomatic.InstanceID]
	if _, previous, err := store.CancelJob(ctx, nextAutomatic.ID); err != nil || previous != domain.JobQueued {
		t.Fatalf("cancel queued automatic update: previous=%s err=%v", previous, err)
	}
	canceledInstance, err := store.GetInstance(ctx, nextAutomatic.InstanceID)
	if err != nil || canceledInstance.Status != beforeCancel {
		t.Fatalf("queued automatic cancellation changed state: %#v err=%v", canceledInstance, err)
	}

	remaining, err := store.JobsForDispatch(ctx, nodeID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var failedCandidate domain.Job
	for _, job := range remaining {
		if isAutomaticMaintenanceUpdate(job) {
			failedCandidate = job
			break
		}
	}
	if failedCandidate.ID == "" {
		t.Fatal("queued cancellation did not release the final automatic update")
	}
	if marked, err := store.MarkDispatched(ctx, failedCandidate.ID); err != nil || !marked {
		t.Fatalf("claim failed automatic candidate: marked=%v err=%v", marked, err)
	}
	if _, err := store.UpdateJob(ctx, failedCandidate.ID, domain.JobRunning, 20, "maintenance running", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	failed, err := store.UpdateJob(ctx, failedCandidate.ID, domain.JobFailed, 30, "steam unavailable", map[string]any{})
	if err != nil || !jobProjectionPending(failed) {
		t.Fatalf("persist failed automatic update: %#v err=%v", failed, err)
	}
	if _, applied, err := store.ApplyPendingJobProjection(ctx, failedCandidate.ID); err != nil || !applied {
		t.Fatalf("project failed automatic update: applied=%v err=%v", applied, err)
	}
	degraded, err := store.GetInstance(ctx, failedCandidate.InstanceID)
	if err != nil || degraded.Status != domain.InstanceDegraded {
		t.Fatalf("failed automatic update did not degrade instance: %#v err=%v", degraded, err)
	}

	// A check that started before a newer install outcome must not resurrect
	// update_available when its delayed terminal frame is projected afterwards.
	staleProbeTarget := create(nodeID, "maintenance-stale-probe", domain.InstanceStopped, "", true)
	staleProbe, _, err := store.QueueSystemProbe(ctx, monitoringTarget{InstanceID: staleProbeTarget.ID, NodeID: nodeID}, "check_update",
		"maintenance-stale-check")
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkDispatched(ctx, staleProbe.ID); err != nil || !marked {
		t.Fatalf("dispatch stale check: marked=%v err=%v", marked, err)
	}
	if _, err := store.UpdateJob(ctx, staleProbe.ID, domain.JobRunning, 25, "checking", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	newerUpdate, _, err := store.QueueInstanceJob(ctx, staleProbeTarget.ID, "update", map[string]any{}, ownerID, "maintenance-newer-update")
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkDispatched(ctx, newerUpdate.ID); err != nil || !marked {
		t.Fatalf("dispatch newer update: marked=%v err=%v", marked, err)
	}
	if _, err := store.UpdateJob(ctx, newerUpdate.ID, domain.JobRunning, 50, "updating", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	newerTerminal, err := store.UpdateJob(ctx, newerUpdate.ID, domain.JobSucceeded, 100, "updated",
		map[string]any{"build_id": "newer-build", "was_running": false})
	if err != nil || !jobProjectionPending(newerTerminal) {
		t.Fatalf("persist newer update: %#v err=%v", newerTerminal, err)
	}
	if _, applied, err := store.ApplyPendingJobProjection(ctx, newerUpdate.ID); err != nil || !applied {
		t.Fatalf("project newer update: applied=%v err=%v", applied, err)
	}
	staleTerminal, err := store.UpdateJob(ctx, staleProbe.ID, domain.JobSucceeded, 100, "stale available result",
		map[string]any{"available": true, "installed_build": "old-build", "available_build": "new-build"})
	if err != nil || !jobProjectionPending(staleTerminal) {
		t.Fatalf("persist stale check: %#v err=%v", staleTerminal, err)
	}
	if _, applied, err := store.ApplyPendingJobProjection(ctx, staleProbe.ID); err != nil || !applied {
		t.Fatalf("project stale check: applied=%v err=%v", applied, err)
	}
	var staleAvailable bool
	if err := store.db.QueryRowContext(ctx, `SELECT available FROM instance_update_state WHERE instance_id=?1`, staleProbeTarget.ID).Scan(&staleAvailable); err != nil {
		t.Fatal(err)
	}
	if staleAvailable {
		t.Fatal("stale pre-update check resurrected update availability")
	}
}

func testMaintenanceProbeInterlock(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	window := time.Date(2026, time.August, 2, 20, 0, 0, 0, time.UTC)
	for index := 0; index < 6; index++ {
		nodeID := uuid.NewString()
		if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,?2,'online')`, nodeID,
			fmt.Sprintf("maintenance-interlock-%d", index)); err != nil {
			t.Fatal(err)
		}
		instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID,
			Name: fmt.Sprintf("maintenance-interlock-%d", index), Port: 32000 + index,
			Config: domain.InstanceConfig{Hostname: "maintenance-interlock", MaxPlayers: 8, MaintenanceCron: "0 4 * * *"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO instance_update_state(instance_id,installed_build,available_build,available)
			VALUES(?1,'old','new',true)`, instance.ID); err != nil {
			t.Fatal(err)
		}
		instance, err = store.GetInstance(ctx, instance.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidate := maintenanceUpdateCandidate{Instance: instance, Cron: instance.Config.MaintenanceCron}
		start := make(chan struct{})
		var wait sync.WaitGroup
		var batch maintenanceBatchResult
		var batchErr, probeErr error
		var probe domain.Job
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			batch, batchErr = store.QueueAutomaticMaintenanceBatch(ctx, nodeID, window, []maintenanceUpdateCandidate{candidate})
		}()
		go func() {
			defer wait.Done()
			<-start
			probe, _, probeErr = store.QueueSystemProbe(ctx, monitoringTarget{InstanceID: instance.ID, NodeID: nodeID}, "check_update",
				fmt.Sprintf("maintenance-interlock-probe-%d", index))
		}()
		close(start)
		wait.Wait()
		if batchErr != nil || (probeErr != nil && !errors.Is(probeErr, ErrConflict)) {
			t.Fatalf("maintenance/probe interlock iteration %d: batch=%v probe=%v", index, batchErr, probeErr)
		}
		if len(batch.Jobs) == 1 {
			if !errors.Is(probeErr, ErrConflict) || probe.ID != "" {
				t.Fatalf("batch winner also admitted probe: batch=%#v probe=%#v err=%v", batch, probe, probeErr)
			}
		} else if len(batch.Jobs) == 0 {
			if probeErr != nil || probe.ID == "" {
				t.Fatalf("probe winner was not persisted: batch=%#v probe=%#v err=%v", batch, probe, probeErr)
			}
		} else {
			t.Fatalf("unexpected interlock batch size: %#v", batch)
		}
		var active int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE instance_id=?1
			AND type IN ('update','check_update') AND status IN ('queued','dispatched','running')`, instance.ID).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 1 {
			t.Fatalf("maintenance/probe interlock admitted %d concurrent jobs", active)
		}
	}
}

func testLateCancelTerminalOutcome(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'late-cancel','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "late-cancel", Port: 29212,
		Config: domain.InstanceConfig{Hostname: "late-cancel", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "update", map[string]any{}, ownerID, "late-cancel-running")
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkDispatched(ctx, job.ID); err != nil || !marked {
		t.Fatalf("mark dispatched: marked=%v err=%v", marked, err)
	}
	canceled, previous, err := store.CancelJob(ctx, job.ID)
	if err != nil || previous != domain.JobDispatched || canceled.Result["cancel_pending"] != true || canceled.FinishedAt != nil {
		t.Fatalf("pending cancellation: %#v previous=%s err=%v", canceled, previous, err)
	}
	dispatchable, err := store.JobsForDispatch(ctx, nodeID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundPendingCancel := false
	for _, candidate := range dispatchable {
		if candidate.ID == job.ID && candidate.Status == domain.JobCanceled {
			foundPendingCancel = true
		}
	}
	if !foundPendingCancel {
		t.Fatal("cancel-pending job was not retained in the durable Agent dispatch outbox")
	}
	if _, err := store.UpdateJob(ctx, job.ID, domain.JobRunning, 30, "late progress", nil); !errors.Is(err, errLateCanceledJobUpdate) {
		t.Fatalf("late non-terminal update was not ignored safely: %v", err)
	}
	stillPending, err := store.GetJob(ctx, job.ID)
	if err != nil || stillPending.Status != domain.JobCanceled || stillPending.Result["cancel_pending"] != true {
		t.Fatalf("late progress changed pending cancellation: %#v err=%v", stillPending, err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "validate", map[string]any{}, ownerID, "late-cancel-lock-check"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel-pending job released its write lock early: %v", err)
	}
	terminal, err := store.UpdateJob(ctx, job.ID, domain.JobSucceeded, 100, "completed before cancellation", map[string]any{
		"cancel_pending": true, "build_id": "late-success",
	})
	if err != nil {
		t.Fatalf("late successful outcome was rejected: %v", err)
	}
	if terminal.Status != domain.JobSucceeded || terminal.Result["cancel_pending"] != nil || terminal.FinishedAt == nil {
		t.Fatalf("late terminal outcome did not clear cancellation marker: %#v", terminal)
	}
	if !jobProjectionPending(terminal) {
		t.Fatalf("late successful outcome did not retain its projection lock: %#v", terminal.Result)
	}
	if projected, applied, projectionErr := store.ApplyPendingJobProjection(ctx, terminal.ID); projectionErr != nil || !applied || jobProjectionPending(projected) {
		t.Fatalf("project late successful outcome: %#v applied=%v err=%v", projected, applied, projectionErr)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "validate", map[string]any{}, ownerID, "late-cancel-lock-released"); err != nil {
		t.Fatalf("late terminal outcome did not release write lock: %v", err)
	}

	// When the Agent confirms a cancellation, both the cancel_pending lock and
	// the instance projection must change in one commit. An ACK replay after new
	// work starts must return the immutable terminal row without projecting the
	// old cancellation again.
	canceledTarget, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "late-cancel-confirmed", Port: 29213,
		Config: domain.InstanceConfig{Hostname: "late-cancel-confirmed", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, canceledTarget.ID); err != nil {
		t.Fatal(err)
	}
	confirmedJob, _, err := store.QueueInstanceJob(ctx, canceledTarget.ID, "update", map[string]any{}, ownerID, "late-cancel-confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if marked, markErr := store.MarkDispatched(ctx, confirmedJob.ID); markErr != nil || !marked {
		t.Fatalf("dispatch confirmed cancellation: marked=%v err=%v", marked, markErr)
	}
	if _, _, err := store.CancelJob(ctx, confirmedJob.ID); err != nil {
		t.Fatalf("request confirmed cancellation: %v", err)
	}
	confirmed, err := store.UpdateJob(ctx, confirmedJob.ID, domain.JobCanceled, 60, "Agent stopped canceled work", map[string]any{"agent_confirmed": true})
	if err != nil || confirmed.Result["cancel_pending"] != nil || confirmed.FinishedAt == nil {
		t.Fatalf("confirm Agent cancellation atomically: %#v err=%v", confirmed, err)
	}
	confirmedProjection, err := store.GetInstance(ctx, canceledTarget.ID)
	if err != nil || confirmedProjection.Status != domain.InstanceDegraded {
		t.Fatalf("Agent cancellation projection was not atomic: %#v err=%v", confirmedProjection, err)
	}
	newWork, _, err := store.QueueInstanceJob(ctx, canceledTarget.ID, "start", map[string]any{}, ownerID, "late-cancel-after-confirmation")
	if err != nil {
		t.Fatalf("queue work after confirmed cancellation: %v", err)
	}
	if replay, replayErr := store.UpdateJob(ctx, confirmedJob.ID, domain.JobCanceled, 60, "Agent stopped canceled work", map[string]any{"agent_confirmed": true}); replayErr != nil || replay.ID != confirmedJob.ID {
		t.Fatalf("replay confirmed cancellation: %#v err=%v", replay, replayErr)
	}
	confirmedProjection, err = store.GetInstance(ctx, canceledTarget.ID)
	if err != nil || confirmedProjection.Status != domain.InstanceStarting {
		t.Fatalf("terminal cancellation replay overwrote newer work: %#v err=%v", confirmedProjection, err)
	}
	if _, _, err := store.CancelJob(ctx, newWork.ID); err != nil {
		t.Fatalf("clean up post-cancellation work: %v", err)
	}

	queued, _, err := store.QueueInstanceJob(ctx, instance.ID, "a2s.info", map[string]any{}, ownerID, "ordinary-cancel")
	if err != nil {
		t.Fatal(err)
	}
	ordinary, previous, err := store.CancelJob(ctx, queued.ID)
	if err != nil || previous != domain.JobQueued || ordinary.Result["cancel_pending"] != nil {
		t.Fatalf("ordinary cancellation: %#v previous=%s err=%v", ordinary, previous, err)
	}
	if _, err := store.UpdateJob(ctx, queued.ID, domain.JobSucceeded, 100, "must not revive", map[string]any{}); err == nil {
		t.Fatal("ordinary canceled job accepted a late success")
	}
}

func testIdempotencyRequestEquivalence(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeA, nodeB := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'fingerprint-a','online'),(?2,'fingerprint-b','online')`, nodeA, nodeB); err != nil {
		t.Fatal(err)
	}
	instanceA, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeA, Name: "fingerprint-a", Port: 29210,
		Config: domain.InstanceConfig{Hostname: "A", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	instanceB, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeB, Name: "fingerprint-b", Port: 29211,
		Config: domain.InstanceConfig{Hostname: "B", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id IN (?1,?2)`, instanceA.ID, instanceB.ID); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{"probe": "first"}
	job, reused, err := store.QueueInstanceJob(ctx, instanceA.ID, "a2s.info", payload, ownerID, "equivalent-job")
	if err != nil || reused {
		t.Fatalf("queue fingerprint job: reused=%v err=%v", reused, err)
	}
	replay, reused, err := store.QueueInstanceJob(ctx, instanceA.ID, "a2s.info", map[string]any{"probe": "first"}, ownerID, "equivalent-job")
	if err != nil || !reused || replay.ID != job.ID {
		t.Fatalf("equivalent replay: %#v reused=%v err=%v", replay, reused, err)
	}
	for name, candidate := range []struct {
		instanceID, action string
		payload            map[string]any
	}{
		{instanceB.ID, "a2s.info", map[string]any{"probe": "first"}},
		{instanceA.ID, "a2s.players", map[string]any{"probe": "first"}},
		{instanceA.ID, "a2s.info", map[string]any{"probe": "changed"}},
	} {
		if _, _, queueErr := store.QueueInstanceJob(ctx, candidate.instanceID, candidate.action, candidate.payload, ownerID, "equivalent-job"); !errors.Is(queueErr, ErrConflict) {
			t.Fatalf("changed request %d (%s) reused the original key: %v", name, candidate.action, queueErr)
		}
	}

	plaintext := map[string]any{"command": "status"}
	secretFingerprint, err := instanceJobRequestFingerprint(instanceA.ID, "rcon", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	sealedFirst, sealedRetry := cloneMap(plaintext), cloneMap(plaintext)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	if err := sealJobPayloadSecrets(masterKey, "rcon", sealedFirst); err != nil {
		t.Fatal(err)
	}
	if err := sealJobPayloadSecrets(masterKey, "rcon", sealedRetry); err != nil {
		t.Fatal(err)
	}
	secretJob, reused, err := store.QueueInstanceJobWithFingerprint(ctx, instanceA.ID, "rcon", sealedFirst, ownerID, "equivalent-secret", secretFingerprint)
	if err != nil || reused {
		t.Fatalf("queue secret job: reused=%v err=%v", reused, err)
	}
	secretReplay, reused, err := store.QueueInstanceJobWithFingerprint(ctx, instanceA.ID, "rcon", sealedRetry, ownerID, "equivalent-secret", secretFingerprint)
	if err != nil || !reused || secretReplay.ID != secretJob.ID {
		t.Fatalf("secret replay: %#v reused=%v err=%v", secretReplay, reused, err)
	}
	changedSecretFingerprint, err := instanceJobRequestFingerprint(instanceA.ID, "rcon", map[string]any{"command": "quit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueInstanceJobWithFingerprint(ctx, instanceA.ID, "rcon", sealedRetry, ownerID, "equivalent-secret", changedSecretFingerprint); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed secret reused original key: %v", err)
	}

	if err := store.SaveInstanceSecrets(ctx, instanceB.ID, "rcon-ciphertext", ""); err != nil {
		t.Fatal(err)
	}
	config := instanceB.Config
	config.Hostname = "updated"
	configFingerprint, err := canonicalRequestFingerprint("instance-config", instanceB.ID, "configure", map[string]any{
		"name": instanceB.Name, "config": config, "server_password": "first-password", "version": instanceB.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCiphertext := "random-ciphertext-one"
	firstRCONCiphertext := "rcon-ciphertext-one"
	updated, configureJob, reused, err := store.UpdateInstanceAndQueueWithFingerprint(ctx, instanceB.ID, instanceB.Name, &config,
		instanceB.Version, ownerID, "equivalent-config", &firstRCONCiphertext, &firstCiphertext, configFingerprint)
	if err != nil || reused {
		t.Fatalf("queue configure job: reused=%v err=%v", reused, err)
	}
	secondCiphertext := "random-ciphertext-two"
	secondRCONCiphertext := "rcon-ciphertext-two"
	replayedInstance, replayedConfigure, reused, err := store.UpdateInstanceAndQueueWithFingerprint(ctx, instanceB.ID, instanceB.Name,
		&config, instanceB.Version, ownerID, "equivalent-config", &secondRCONCiphertext, &secondCiphertext, configFingerprint)
	if err != nil || !reused || replayedConfigure.ID != configureJob.ID || replayedInstance.Version != updated.Version {
		t.Fatalf("configure replay: instance=%#v job=%#v reused=%v err=%v", replayedInstance, replayedConfigure, reused, err)
	}
	storedRCON, storedServer, err := store.InstanceSecrets(ctx, instanceB.ID)
	if err != nil || storedRCON != firstRCONCiphertext || storedServer != firstCiphertext {
		t.Fatalf("configure secret update = rcon %q server %q err=%v", storedRCON, storedServer, err)
	}
	changedConfigFingerprint, err := canonicalRequestFingerprint("instance-config", instanceB.ID, "configure", map[string]any{
		"name": instanceB.Name, "config": config, "server_password": "second-password", "version": instanceB.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.UpdateInstanceAndQueueWithFingerprint(ctx, instanceB.ID, instanceB.Name, &config, instanceB.Version,
		ownerID, "equivalent-config", nil, &secondCiphertext, changedConfigFingerprint); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed config secret reused original key: %v", err)
	}

	legacyKey := "legacy-without-fingerprint"
	legacyID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by)
		VALUES(?1,?2,?3,'a2s.info','succeeded','{}',?4,?5)`, legacyID, nodeA, instanceA.ID, legacyKey, ownerID); err != nil {
		t.Fatal(err)
	}
	legacyFingerprint, err := instanceJobRequestFingerprint(instanceA.ID, "a2s.info", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.JobByIdempotencyKeyMatching(ctx, ownerID, legacyKey, legacyFingerprint); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy job without fingerprint was replayed: %v", err)
	}
	var persistedFingerprint string
	if err := store.db.QueryRowContext(ctx, `SELECT request_fingerprint FROM jobs WHERE id=?1`, secretJob.ID).Scan(&persistedFingerprint); err != nil {
		t.Fatal(err)
	}
	if persistedFingerprint != secretFingerprint || strings.Contains(persistedFingerprint, "status") {
		t.Fatalf("unexpected persisted request fingerprint %q", persistedFingerprint)
	}
}

func testBackupUploadLifecycle(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'upload-node','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, Name: "upload-target", Port: 29116,
		Config: domain.InstanceConfig{Hostname: "target", Mode: "coop", Difficulty: "normal", MaxPlayers: 8,
			StartMap: "c1m1_hotel", PublicStatus: true, MaintenanceCron: "0 4 * * *"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	raw := []byte("portable backup archive")
	digest := sha256.Sum256(raw)
	fileSHA := hex.EncodeToString(digest[:])
	upload, reused, err := store.CreateBackupUpload(ctx, instance.ID, "portable.zip", "linux/amd64", int64(len(raw)), fileSHA, ownerID, "create-upload")
	if err != nil || reused {
		t.Fatalf("create backup upload: reused=%v err=%v", reused, err)
	}
	replayed, reused, err := store.CreateBackupUpload(ctx, instance.ID, "portable.zip", "linux/amd64", int64(len(raw)), fileSHA, ownerID, "create-upload")
	if err != nil || !reused || replayed.ID != upload.ID {
		t.Fatalf("replay backup upload: %#v reused=%v err=%v", replayed, reused, err)
	}
	chunkPayload := map[string]any{"upload_id": upload.ID, "offset": int64(0), "length": len(raw),
		"chunk_sha256": fileSHA, "expected_size": len(raw), "expected_sha256": fileSHA,
		"content_base64": encryptedValuePlaceholder, "content_base64_ciphertext": "encrypted-test-body"}
	chunk, reused, err := store.QueueBackupUploadChunk(ctx, instance.ID, upload.ID, chunkPayload, ownerID, "upload-chunk-0")
	if err != nil || reused {
		t.Fatalf("queue upload chunk: reused=%v err=%v", reused, err)
	}
	if replay, wasReused, err := store.QueueBackupUploadChunk(ctx, instance.ID, upload.ID, chunkPayload, ownerID, "upload-chunk-0"); err != nil || !wasReused || replay.ID != chunk.ID {
		t.Fatalf("replay upload chunk: %#v reused=%v err=%v", replay, wasReused, err)
	}
	if marked, err := store.MarkDispatched(ctx, chunk.ID); err != nil || !marked {
		t.Fatalf("dispatch upload chunk: marked=%v err=%v", marked, err)
	}
	completed, err := store.UpdateJob(ctx, chunk.ID, domain.JobSucceeded, 100, "uploaded", map[string]any{
		"upload_id": upload.ID, "offset": int64(0), "length": len(raw), "received_size": int64(len(raw)),
		"chunk_sha256": fileSHA, "file_sha256": fileSHA, "complete": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay, wasReused, err := store.QueueBackupUploadChunk(ctx, instance.ID, upload.ID, chunkPayload, ownerID, "upload-chunk-unprojected"); err != nil || !wasReused || replay.ID != chunk.ID {
		t.Fatalf("unprojected successful chunk was duplicated: %#v reused=%v err=%v", replay, wasReused, err)
	}
	conflictingChunk := cloneMap(chunkPayload)
	conflictingChunk["chunk_sha256"] = strings.Repeat("a", 64)
	if _, _, err := store.QueueBackupUploadChunk(ctx, instance.ID, upload.ID, conflictingChunk, ownerID, "upload-chunk-unprojected-conflict"); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting chunk raced an unprojected success: %v", err)
	}
	// A terminal Agent update and its projection are deliberately separate so
	// failures can be replayed. Expiry must not win in that small window.
	if _, err := store.db.ExecContext(ctx, `UPDATE backup_uploads SET expires_at=?2 WHERE id=?1`, upload.ID, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.GetBackupUpload(ctx, instance.ID, upload.ID); err != nil || pending.Status != "uploading" {
		t.Fatalf("unprojected successful chunk was expired: %#v err=%v", pending, err)
	}
	if projected, applied, projectionErr := store.ApplyPendingJobProjection(ctx, completed.ID); projectionErr != nil || !applied || jobProjectionPending(projected) {
		t.Fatalf("apply upload chunk: %#v applied=%v err=%v", projected, applied, projectionErr)
	}
	upload, err = store.GetBackupUpload(ctx, instance.ID, upload.ID)
	if err != nil || upload.Status != "ready" || upload.ReceivedSize != upload.Size {
		t.Fatalf("ready upload projection: %#v err=%v", upload, err)
	}
	storedChunk, err := store.GetJob(ctx, chunk.ID)
	if err != nil || storedChunk.Payload["content_base64_ciphertext"] != nil {
		t.Fatalf("terminal chunk retained encrypted body: %#v err=%v", storedChunk.Payload, err)
	}
	restorePayload := map[string]any{"upload_id": upload.ID, "expected_size": upload.Size, "expected_sha256": upload.SHA256,
		"source_platform": upload.Platform, "instance": instance}
	restore, reused, err := store.QueueBackupUploadRestore(ctx, instance.ID, upload.ID, restorePayload, ownerID, "restore-upload")
	if err != nil || reused {
		t.Fatalf("queue uploaded restore: reused=%v err=%v", reused, err)
	}
	if marked, err := store.MarkDispatched(ctx, restore.ID); err != nil || !marked {
		t.Fatalf("dispatch uploaded restore: marked=%v err=%v", marked, err)
	}
	failed, err := store.UpdateJob(ctx, restore.ID, domain.JobFailed, 100, "restore rolled back", map[string]any{"upload_id": upload.ID, "upload_cleaned": false})
	if err != nil {
		t.Fatal(err)
	}
	if replay, wasReused, err := store.QueueBackupUploadRestore(ctx, instance.ID, upload.ID, restorePayload, ownerID, "restore-upload"); err != nil || !wasReused || replay.ID != restore.ID {
		t.Fatalf("same key did not replay failed restore: %#v reused=%v err=%v", replay, wasReused, err)
	}
	upload, err = store.GetBackupUpload(ctx, instance.ID, upload.ID)
	if err != nil || upload.Status != "failed" || upload.RestoreJobID != restore.ID || upload.ExpiresAt.Before(time.Now().UTC().Add(23*time.Hour)) {
		t.Fatalf("failed restore was not projected atomically with its terminal job: %#v err=%v", upload, err)
	}
	if err := store.ApplyJobFailure(ctx, failed); err != nil {
		t.Fatalf("replay failed uploaded restore projection: %v", err)
	}
	upload, err = store.GetBackupUpload(ctx, instance.ID, upload.ID)
	if err != nil || upload.Status != "failed" || upload.RestoreJobID != restore.ID || upload.ExpiresAt.Before(time.Now().UTC().Add(23*time.Hour)) {
		t.Fatalf("failed upload restore projection: %#v err=%v", upload, err)
	}
	if replay, wasReused, err := store.QueueBackupUploadRestore(ctx, instance.ID, upload.ID, restorePayload, ownerID, "restore-upload"); err != nil || !wasReused || replay.ID != restore.ID {
		t.Fatalf("same key must replay failed restore: %#v reused=%v err=%v", replay, wasReused, err)
	}
	retry, reused, err := store.QueueBackupUploadRestore(ctx, instance.ID, upload.ID, restorePayload, ownerID, "restore-upload-retry")
	if err != nil || reused || retry.ID == restore.ID {
		t.Fatalf("retry uploaded restore: %#v reused=%v err=%v", retry, reused, err)
	}
	if marked, err := store.MarkDispatched(ctx, retry.ID); err != nil || !marked {
		t.Fatalf("dispatch retried upload restore: marked=%v err=%v", marked, err)
	}
	successfulRestoreResult := map[string]any{
		"upload_id": upload.ID, "upload_cleaned": true, "format_version": 2, "launch_config_restored": true,
		"restored_launch_config": map[string]any{"hostname": "portable", "mode": "versus", "difficulty": "hard",
			"max_players": 12, "start_map": "c5m1_waterfront", "region": "3", "steam_group_id": "12345",
			"extra_args": []any{"-strictportbind"}, "cvars": map[string]any{"sv_tags": "restored"}, "auto_restart": true},
	}
	finished, err := store.UpdateJob(ctx, retry.ID, domain.JobSucceeded, 100, "restored", successfulRestoreResult)
	if err != nil {
		t.Fatal(err)
	}
	upload, err = store.GetBackupUpload(ctx, instance.ID, upload.ID)
	if err != nil || upload.Status != "succeeded" || upload.RestoreJobID != retry.ID {
		t.Fatalf("successful restore upload state was not committed atomically: %#v err=%v", upload, err)
	}
	projected, err := store.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Version != instance.Version+1 || projected.Config.Hostname != "portable" || projected.Config.Mode != "versus" ||
		projected.Config.MaxPlayers != 12 || !projected.Config.AutoRestart || !projected.Config.PublicStatus ||
		projected.Config.MaintenanceCron != "0 4 * * *" {
		t.Fatalf("restored launch configuration was not committed atomically: %#v", projected)
	}
	if replayedJob, replayErr := store.UpdateJob(ctx, retry.ID, domain.JobSucceeded, 100, "restored", successfulRestoreResult); replayErr != nil || replayedJob.ID != retry.ID {
		t.Fatalf("terminal restore replay was not idempotent: %#v err=%v", replayedJob, replayErr)
	}
	changedRestoreResult := cloneMap(successfulRestoreResult)
	changedRestoreResult["upload_cleaned"] = false
	if _, replayErr := store.UpdateJob(ctx, retry.ID, domain.JobSucceeded, 100, "restored", changedRestoreResult); !errors.Is(replayErr, errTerminalOutcomeConflict) {
		t.Fatalf("changed terminal restore replay was not rejected: %v", replayErr)
	}
	persistedRestore, err := store.GetJob(ctx, retry.ID)
	if err != nil || persistedRestore.Result["upload_cleaned"] != true {
		t.Fatalf("changed terminal replay overwrote persisted result: %#v err=%v", persistedRestore.Result, err)
	}
	if err := store.ApplyJobResult(ctx, finished); err != nil {
		t.Fatalf("replay uploaded restore projection: %v", err)
	}
	upload, err = store.GetBackupUpload(ctx, instance.ID, upload.ID)
	if err != nil || upload.Status != "succeeded" || upload.RestoreJobID != retry.ID {
		t.Fatalf("restored upload projection: %#v err=%v", upload, err)
	}
	projected, err = store.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Version != instance.Version+1 || projected.Config.Hostname != "portable" || projected.Config.Mode != "versus" ||
		projected.Config.MaxPlayers != 12 || !projected.Config.AutoRestart || !projected.Config.PublicStatus ||
		projected.Config.MaintenanceCron != "0 4 * * *" {
		t.Fatalf("restored launch configuration projection: %#v", projected)
	}
	if err := store.ApplyJobResult(ctx, finished); err != nil {
		t.Fatalf("replay restored upload projection: %v", err)
	}
	replayedProjection, err := store.GetInstance(ctx, instance.ID)
	if err != nil || replayedProjection.Version != projected.Version {
		t.Fatalf("restore replay incremented configuration twice: %#v err=%v", replayedProjection, err)
	}
	queuedRestore, reused, err := store.QueueInstanceJob(ctx, instance.ID, "restore", map[string]any{"archive": "unused.zip"}, ownerID, "queued-restore-cancel")
	if err != nil || reused {
		t.Fatalf("queue cancelable restore: reused=%v err=%v", reused, err)
	}
	canceledRestore, previous, err := store.CancelJob(ctx, queuedRestore.ID)
	if err != nil || previous != domain.JobQueued || canceledRestore.Status != domain.JobCanceled {
		t.Fatalf("cancel queued restore atomically: %#v previous=%s err=%v", canceledRestore, previous, err)
	}
	canceledInstance, err := store.GetInstance(ctx, instance.ID)
	if err != nil || canceledInstance.Status != domain.InstanceDegraded {
		t.Fatalf("queued restore cancellation projection was not atomic: %#v err=%v", canceledInstance, err)
	}
	if replayedCancel, previous, replayErr := store.CancelJob(ctx, canceledRestore.ID); replayErr != nil ||
		previous != domain.JobCanceled || replayedCancel.ID != canceledRestore.ID {
		t.Fatalf("queued restore cancellation replay: %#v previous=%s err=%v", replayedCancel, previous, replayErr)
	}
}

func testConfirmedQuarantinePurge(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'quarantine-node','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{
		NodeID: nodeID, Name: "retired-server", Port: 29115, Config: domain.InstanceConfig{MaxPlayers: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	deleteJob, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "quarantine"}, ownerID, "integration-quarantine-delete")
	if err != nil {
		t.Fatal(err)
	}
	quarantinedAt := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	entryName := instance.ID + "-" + quarantinedAt.Format("20060102T150405Z")
	deleteJob.Status = domain.JobSucceeded
	deleteJob.Result = map[string]any{"quarantine_entry": entryName, "root_index": 2, "retention_days": 7}
	if err := store.ApplyJobResult(ctx, deleteJob); err != nil {
		t.Fatalf("project quarantine result: %v", err)
	}
	var projectedEntryID string
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM quarantine_entries WHERE source_job_id=?1`, deleteJob.ID).Scan(&projectedEntryID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateQuarantinePurgeConfirmation(ctx, nodeID, projectedEntryID, ownerID); !errors.Is(err, ErrConflict) {
		t.Fatalf("fresh quarantine entry accepted for permanent purge: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE quarantine_entries SET quarantined_at=?2,retention_expires_at=?3 WHERE source_job_id=?1`,
		deleteJob.ID, quarantinedAt, quarantinedAt.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListQuarantineEntries(ctx, nodeID)
	if err != nil || len(items) != 1 {
		t.Fatalf("quarantine items = %#v, err=%v", items, err)
	}
	entry := items[0]
	if entry.EntryName != entryName || entry.RootIndex != 2 || entry.OriginalInstanceID != instance.ID ||
		!entry.RetentionExpiresAt.Equal(quarantinedAt.Add(7*24*time.Hour)) {
		t.Fatalf("unexpected quarantine projection: %#v", entry)
	}
	var persistedResult map[string]any
	var persistedResultRaw []byte
	if err := store.db.QueryRowContext(ctx, `SELECT result FROM jobs WHERE id=?1`, deleteJob.ID).Scan(&persistedResultRaw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(persistedResultRaw, &persistedResult); err != nil {
		t.Fatal(err)
	}
	if _, leaked := persistedResult["quarantine_path"]; leaked || stringFromAny(persistedResult["quarantine_id"]) != entry.ID {
		t.Fatalf("unsafe or incomplete delete result: %#v", persistedResult)
	}

	confirmation, expiresAt, err := store.CreateQuarantinePurgeConfirmation(ctx, nodeID, entry.ID, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(confirmation, "qpc_") || time.Until(expiresAt) > 6*time.Minute {
		t.Fatalf("unexpected confirmation token metadata: %q %s", confirmation, expiresAt)
	}
	purgeJob, reused, err := store.QueueQuarantinePurge(ctx, nodeID, entry.ID, ownerID, confirmation, "integration-quarantine-purge")
	if err != nil || reused {
		t.Fatalf("queue confirmed purge: reused=%v err=%v", reused, err)
	}
	if purgeJob.InstanceID != "" || purgeJob.Type != "purge_quarantine" || stringFromAny(purgeJob.Payload["quarantine_entry"]) != entryName {
		t.Fatalf("unsafe purge job: %#v", purgeJob)
	}
	replayed, reused, err := store.QueueQuarantinePurge(ctx, nodeID, entry.ID, ownerID, confirmation, "integration-quarantine-purge")
	if err != nil || !reused || replayed.ID != purgeJob.ID {
		t.Fatalf("idempotent confirmation replay: job=%#v reused=%v err=%v", replayed, reused, err)
	}
	if _, _, err := store.QueueQuarantinePurge(ctx, nodeID, entry.ID, ownerID, confirmation, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("consumed token without matching idempotency key was accepted: %v", err)
	}
	canceledPurge, previous, err := store.CancelJob(ctx, purgeJob.ID)
	if err != nil || previous != domain.JobQueued || canceledPurge.Status != domain.JobCanceled {
		t.Fatalf("cancel queued quarantine purge: %#v previous=%s err=%v", canceledPurge, previous, err)
	}
	var purgePointer string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(purge_job_id,'') FROM quarantine_entries WHERE id=?1`, entry.ID).Scan(&purgePointer); err != nil {
		t.Fatal(err)
	}
	if purgePointer != "" {
		t.Fatalf("queued purge cancellation did not atomically release quarantine pointer: %s", purgePointer)
	}

	retryConfirmation, _, err := store.CreateQuarantinePurgeConfirmation(ctx, nodeID, entry.ID, ownerID)
	if err != nil {
		t.Fatalf("create retry purge confirmation: %v", err)
	}
	purgeJob, reused, err = store.QueueQuarantinePurge(ctx, nodeID, entry.ID, ownerID, retryConfirmation, "integration-quarantine-purge-retry")
	if err != nil || reused {
		t.Fatalf("retry confirmed purge: %#v reused=%v err=%v", purgeJob, reused, err)
	}
	if replayedCancel, replayPrevious, replayErr := store.CancelJob(ctx, canceledPurge.ID); replayErr != nil ||
		replayPrevious != domain.JobCanceled || replayedCancel.ID != canceledPurge.ID {
		t.Fatalf("replay old purge cancellation: %#v previous=%s err=%v", replayedCancel, replayPrevious, replayErr)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(purge_job_id,'') FROM quarantine_entries WHERE id=?1`, entry.ID).Scan(&purgePointer); err != nil {
		t.Fatal(err)
	}
	if purgePointer != purgeJob.ID {
		t.Fatalf("old purge cancellation cleared newer work: pointer=%s job=%s", purgePointer, purgeJob.ID)
	}
	purgeJob.Status = domain.JobSucceeded
	if err := store.ApplyJobResult(ctx, purgeJob); err != nil {
		t.Fatalf("project successful purge: %v", err)
	}
	items, err = store.ListQuarantineEntries(ctx, nodeID)
	if err != nil || len(items) != 0 {
		t.Fatalf("purged quarantine item remained active: %#v err=%v", items, err)
	}
}

func testPublicInstanceProjection(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status,public_address)
		VALUES(?1,'public-node','online','game.example.com')`, nodeID); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{
		NodeID: nodeID, Name: "public-instance", Port: 28115,
		Config: domain.InstanceConfig{Hostname: "Public Campaign", Mode: "coop", MaxPlayers: 8, PublicStatus: true},
	})
	if err != nil {
		t.Fatalf("create public instance: %v", err)
	}
	if instance.NodeName != "public-node" || instance.NodeStatus != domain.NodeOnline {
		t.Fatalf("created instance node projection = %q/%q, want public-node/online", instance.NodeName, instance.NodeStatus)
	}
	projected, err := store.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatalf("read public instance projection: %v", err)
	}
	if projected.NodePublicAddress != "game.example.com" {
		t.Fatalf("instance node_public_address = %q, want game.example.com", projected.NodePublicAddress)
	}
	if projected.NodeName != "public-node" || projected.NodeStatus != domain.NodeOnline {
		t.Fatalf("instance node projection = %q/%q, want public-node/online", projected.NodeName, projected.NodeStatus)
	}
	if err := store.SetNodeOffline(ctx, nodeID); err != nil {
		t.Fatalf("set node offline: %v", err)
	}
	instances, err := store.ListInstances(ctx, nodeID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("offline node instances = %#v, err=%v", instances, err)
	}
	if instances[0].NodeName != "public-node" || instances[0].NodeStatus != domain.NodeOffline {
		t.Fatalf("offline instance node projection = %q/%q, want public-node/offline", instances[0].NodeName, instances[0].NodeStatus)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='running',mode='' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	items, err := store.PublicServers(ctx)
	if err != nil {
		t.Fatalf("list public servers: %v", err)
	}
	for _, item := range items {
		if item.ID != instance.ID {
			continue
		}
		if item.Mode != "coop" {
			t.Fatalf("public mode = %q, want config fallback coop", item.Mode)
		}
		if item.Address != "game.example.com:28115" {
			t.Fatalf("public address = %q", item.Address)
		}
		return
	}
	t.Fatal("public instance was omitted from status projection")
}

func testReadAndWriteJobLocks(t *testing.T, ctx context.Context, store *Store, ownerID string) {
	t.Helper()
	nodeID, instanceID := uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'integration-node','online')`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO instances(id,node_id,name,status,port,max_players,config)
		VALUES(?1,?2,'integration-instance','stopped',27015,8,'{}')`, instanceID, nodeID); err != nil {
		t.Fatal(err)
	}
	target := monitoringTarget{NodeID: nodeID, InstanceID: instanceID}
	if _, _, err := store.QueueSystemProbe(ctx, target, "check_update", "integration-update-probe"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instanceID, "update", map[string]any{}, ownerID, "integration-write"); err != nil {
		t.Fatalf("read-only update probe blocked a write job: %v", err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instanceID, "file", map[string]any{"operation": "read", "relative_path": "left4dead2/cfg/server.cfg"}, ownerID, "integration-read"); err != nil {
		t.Fatalf("file read was incorrectly treated as a write job: %v", err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instanceID, "file", map[string]any{"operation": "write", "relative_path": "left4dead2/cfg/server.cfg"}, ownerID, "integration-second-write"); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent write job was not rejected: %v", err)
	}

	var active int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE instance_id=?1 AND status='queued'`, instanceID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 3 {
		t.Fatalf("queued read/write jobs = %d, want 3", active)
	}
}

func testInstancePortReservations(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES(?1,'port-node','online')`, nodeID); err != nil {
		t.Fatal(err)
	}

	first, err := store.CreateInstance(ctx, CreateInstanceInput{
		NodeID: nodeID, Name: "first", Port: 27016, TVPort: 27020,
		Config: domain.InstanceConfig{MaxPlayers: 8},
	})
	if err != nil {
		t.Fatalf("create first instance: %v", err)
	}
	if _, err := store.CreateInstance(ctx, CreateInstanceInput{
		NodeID: nodeID, Name: "game-on-tv", Port: 27020,
		Config: domain.InstanceConfig{MaxPlayers: 8},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("game port reused a SourceTV port: %v", err)
	}
	if _, err := store.CreateInstance(ctx, CreateInstanceInput{
		NodeID: nodeID, Name: "tv-on-game", Port: 27021, TVPort: 27016,
		Config: domain.InstanceConfig{MaxPlayers: 8},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("SourceTV port reused a game port: %v", err)
	}

	automatic, err := store.CreateInstance(ctx, CreateInstanceInput{
		NodeID: nodeID, Name: "automatic", TVPort: 27015,
		Config: domain.InstanceConfig{MaxPlayers: 8},
	})
	if err != nil {
		t.Fatalf("allocate automatic port: %v", err)
	}
	if automatic.Port != 27017 {
		t.Fatalf("automatic port = %d, want 27017 (27015 SourceTV and 27016 game are occupied)", automatic.Port)
	}

	const concurrentCreates = 8
	type createResult struct {
		instance domain.Instance
		err      error
	}
	results := make(chan createResult, concurrentCreates)
	for index := range concurrentCreates {
		go func(index int) {
			instance, createErr := store.CreateInstance(ctx, CreateInstanceInput{
				NodeID: nodeID, Name: fmt.Sprintf("concurrent-%d", index),
				Config: domain.InstanceConfig{MaxPlayers: 8},
			})
			results <- createResult{instance: instance, err: createErr}
		}(index)
	}
	ports := map[int]bool{first.Port: true, first.TVPort: true, automatic.Port: true, automatic.TVPort: true}
	for range concurrentCreates {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent automatic allocation failed: %v", result.err)
		}
		if ports[result.instance.Port] {
			t.Fatalf("concurrent automatic allocation reused port %d", result.instance.Port)
		}
		ports[result.instance.Port] = true
	}

	if err := store.DeleteInstanceAfterJob(ctx, automatic.ID); err != nil {
		t.Fatalf("delete automatic instance: %v", err)
	}
	var reservations int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM instance_port_reservations WHERE instance_id=?1`, automatic.ID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("deleted instance retained %d port reservations", reservations)
	}
}
