package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const jobColumns = `id,node_id,COALESCE(instance_id,''),COALESCE(game_installation_id,''),type,status,progress,message,payload,result,
	idempotency_key,COALESCE(requested_by,''),created_at,started_at,finished_at`

const jobColumnsWithFingerprint = jobColumns + `,request_fingerprint`

var errLateCanceledJobUpdate = errors.New("late non-terminal update for a pending cancellation")

func scanJob(row scanner) (domain.Job, error) {
	job, _, err := scanJobWithOptionalFingerprint(row, false)
	return job, err
}

func scanJobWithFingerprint(row scanner) (domain.Job, string, error) {
	return scanJobWithOptionalFingerprint(row, true)
}

func scanJobWithOptionalFingerprint(row scanner, withFingerprint bool) (domain.Job, string, error) {
	var job domain.Job
	var payload, result []byte
	var idempotency *string
	var fingerprint *string
	destinations := []any{&job.ID, &job.NodeID, &job.InstanceID, &job.InstallationID, &job.Type, &job.Status, &job.Progress, &job.Message,
		&payload, &result, &idempotency, &job.RequestedBy, &job.CreatedAt, &job.StartedAt, &job.FinishedAt}
	if withFingerprint {
		destinations = append(destinations, &fingerprint)
	}
	err := row.Scan(destinations...)
	if err == nil {
		if idempotency != nil {
			job.IdempotencyKey = *idempotency
		}
		if err = json.Unmarshal(payload, &job.Payload); err == nil {
			err = json.Unmarshal(result, &job.Result)
		}
	}
	if fingerprint == nil {
		return job, "", err
	}
	return job, *fingerprint, err
}

func (s *Store) QueueInstanceJob(ctx context.Context, instanceID, action string, payload map[string]any, requestedBy, idempotencyKey string) (domain.Job, bool, error) {
	fingerprint, err := instanceJobRequestFingerprint(instanceID, action, payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	return s.QueueInstanceJobWithFingerprint(ctx, instanceID, action, payload, requestedBy, idempotencyKey, fingerprint)
}

func (s *Store) QueueInstanceJobWithFingerprint(ctx context.Context, instanceID, action string, payload map[string]any, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if err := validateInstanceAction(action); err != nil {
		return domain.Job{}, false, err
	}
	if len(idempotencyKey) > 200 {
		return domain.Job{}, false, errors.New("idempotency key is too long")
	}
	if err := requireRequestFingerprint(idempotencyKey, requestFingerprint); err != nil {
		return domain.Job{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback()
	if idempotencyKey != "" {
		var existing domain.Job
		var storedFingerprint string
		var lookupErr error
		if requestedBy == "" {
			existing, storedFingerprint, lookupErr = scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by IS NULL AND idempotency_key=?1`, idempotencyKey))
		} else {
			existing, storedFingerprint, lookupErr = scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
		}
		if lookupErr == nil {
			if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
				return domain.Job{}, false, ErrConflict
			}
			return existing, true, tx.Commit()
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.Job{}, false, lookupErr
		}
	}
	var nodeID string
	var instanceStatus domain.InstanceStatus
	var installationID string
	if err := tx.QueryRowContext(ctx, `SELECT node_id,status,COALESCE(installation_id,'') FROM instances WHERE id=?1`, instanceID).Scan(&nodeID, &instanceStatus, &installationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if instanceStatus == domain.InstanceExternal && !externalInstanceJobAllowed(action, payload) {
		return domain.Job{}, false, fmt.Errorf("external instance is read-only until a controlled restart: %w", ErrConflict)
	}
	if action == "restart" && !restartableInstanceStatus(instanceStatus) {
		return domain.Job{}, false, fmt.Errorf("instance must be running before it can restart: %w", ErrConflict)
	}
	if installationID != "" && (action == "start" || action == "restart") {
		var installationStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM game_installations WHERE id=?1`, installationID).Scan(&installationStatus); err != nil {
			return domain.Job{}, false, err
		}
		if installationStatus != "ready" {
			return domain.Job{}, false, fmt.Errorf("game installation is not ready: %w", ErrConflict)
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID, InstallationID: installationID, Type: action, Status: domain.JobQueued,
		Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,game_installation_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,NULLIF(?4,''),?5,?6,?7,NULLIF(?8,''),?9,?10,NULLIF(?11,''))`, job.ID, job.NodeID, job.InstanceID,
		job.InstallationID, job.Type, job.Status, string(raw), job.IdempotencyKey, nullableUUID(job.RequestedBy), job.CreatedAt, requestFingerprint)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	transient := map[string]string{"start": "starting", "stop": "stopping", "restart": "stopping", "update": "updating",
		"validate": "updating", "install": "installing"}
	if status := transient[action]; status != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET status=?2,updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, instanceID, status); err != nil {
			return domain.Job{}, false, err
		}
	}
	if action == "game.import" && installationID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE game_installations SET status='validating',last_error='',updated_at=?2 WHERE id=?1`, installationID, job.CreatedAt); err != nil {
			return domain.Job{}, false, err
		}
	}
	return job, false, tx.Commit()
}

func restartableInstanceStatus(status domain.InstanceStatus) bool {
	return status == domain.InstanceRunning || status == domain.InstanceDegraded || status == domain.InstanceExternal
}

// QueueNodeJob persists a node-scoped operation using the same idempotency and
// reconnect semantics as instance jobs. Concurrent jobs of the same supported
// node operation are coalesced.
func (s *Store) QueueNodeJob(ctx context.Context, nodeID, action string, payload map[string]any, requestedBy, idempotencyKey string) (domain.Job, bool, error) {
	requestFingerprint, err := nodeJobRequestFingerprint(nodeID, action, payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	return s.QueueNodeJobWithFingerprint(ctx, nodeID, action, payload, requestedBy, idempotencyKey, requestFingerprint)
}

func (s *Store) QueueNodeJobWithFingerprint(ctx context.Context, nodeID, action string, payload map[string]any, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if action != "discover_instances" && action != "map.import.qq_flash" {
		return domain.Job{}, false, fmt.Errorf("unsupported node action %q", action)
	}
	if len(idempotencyKey) > 200 {
		return domain.Job{}, false, errors.New("idempotency key is too long")
	}
	if err := requireRequestFingerprint(idempotencyKey, requestFingerprint); err != nil {
		return domain.Job{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback()
	if idempotencyKey != "" {
		var existing domain.Job
		var storedFingerprint string
		var lookupErr error
		if requestedBy == "" {
			existing, storedFingerprint, lookupErr = scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by IS NULL AND idempotency_key=?1`, idempotencyKey))
		} else {
			existing, storedFingerprint, lookupErr = scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
		}
		if lookupErr == nil {
			if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
				return domain.Job{}, false, ErrConflict
			}
			return existing, true, tx.Commit()
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.Job{}, false, lookupErr
		}
	}
	// Serialize scan creation without requiring a schema-level lock. This also
	// closes the race between the active-job lookup and insert.
	var status domain.NodeStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id=?1`, nodeID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if status == domain.NodeRevoked {
		return domain.Job{}, false, ErrUnauthorized
	}
	if existing, lookupErr := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE node_id=?1 AND instance_id IS NULL AND type=?2 AND status IN ('queued','dispatched','running')
		ORDER BY created_at,id LIMIT 1`, nodeID, action)); lookupErr == nil {
		return existing, true, tx.Commit()
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return domain.Job{}, false, lookupErr
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, Type: action, Status: domain.JobQueued, Payload: payload,
		IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,NULL,?3,?4,?5,NULLIF(?6,''),?7,?8,NULLIF(?9,''))`, job.ID, job.NodeID, job.Type, job.Status, string(raw),
		job.IdempotencyKey, nullableUUID(job.RequestedBy), job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}

func externalInstanceJobAllowed(action string, payload map[string]any) bool {
	switch action {
	case "restart", "a2s.info", "a2s.players", "rcon.players", "list", "logs.search", "logs.chunk", "backup", "backup.chunk", "check_update":
		return true
	case "file":
		operation := strings.ToLower(strings.TrimSpace(stringFromAny(payload["operation"])))
		return operation == "" || operation == "read"
	case "delete", "detach":
		return action == "detach" || strings.EqualFold(stringFromAny(payload["mode"]), "detach")
	default:
		return false
	}
}

func (s *Store) GetJob(ctx context.Context, id string) (domain.Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	return job, err
}

// JobByIdempotencyKeyMatching is the only safe preflight lookup for an HTTP
// replay. Legacy jobs without a fingerprint and keys used by a different
// canonical request fail closed with ErrConflict.
func (s *Store) JobByIdempotencyKeyMatching(ctx context.Context, requestedBy, key, expectedFingerprint string) (domain.Job, error) {
	if key == "" {
		return domain.Job{}, ErrNotFound
	}
	if !validRequestFingerprint(expectedFingerprint) {
		return domain.Job{}, errors.New("invalid request fingerprint")
	}
	var row scanner
	if requestedBy == "" {
		row = s.db.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by IS NULL AND idempotency_key=?1`, key)
	} else {
		row = s.db.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, key)
	}
	job, storedFingerprint, err := scanJobWithFingerprint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, err
	}
	if !requestFingerprintsEqual(storedFingerprint, expectedFingerprint) {
		return domain.Job{}, ErrConflict
	}
	return job, nil
}

type jobBackgroundFilter string

const (
	jobBackgroundInclude      jobBackgroundFilter = "include"
	jobBackgroundExclude      jobBackgroundFilter = "exclude"
	jobBackgroundOnly         jobBackgroundFilter = "only"
	backgroundJobSQLPredicate                     = `requested_by IS NULL AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update')`
)

func (s *Store) ListJobs(ctx context.Context, nodeID, instanceID string, background jobBackgroundFilter, limit int) ([]domain.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	backgroundClause := ""
	switch background {
	case jobBackgroundExclude:
		backgroundClause = " AND NOT (" + backgroundJobSQLPredicate + ")"
	case jobBackgroundOnly:
		backgroundClause = " AND (" + backgroundJobSQLPredicate + ")"
	case jobBackgroundInclude:
	default:
		return nil, fmt.Errorf("invalid background job filter %q", background)
	}
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE (?1='' OR node_id=?1) AND (?2='' OR instance_id=?2)
		` + backgroundClause + ` ORDER BY created_at DESC,id DESC LIMIT ?3`
	rows, err := s.db.QueryContext(ctx, query, nodeID, instanceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, job)
	}
	return items, rows.Err()
}

func ValidJobTransition(from, to domain.JobStatus) bool {
	if from == to {
		return true
	}
	switch from {
	case domain.JobQueued:
		return to == domain.JobDispatched || to == domain.JobRunning || to == domain.JobSucceeded || to == domain.JobCanceled || to == domain.JobFailed
	case domain.JobDispatched:
		return to == domain.JobRunning || to == domain.JobSucceeded || to == domain.JobCanceled || to == domain.JobFailed
	case domain.JobRunning:
		return to == domain.JobSucceeded || to == domain.JobFailed || to == domain.JobCanceled
	default:
		return false
	}
}

func validPersistedJobTransition(current domain.Job, to domain.JobStatus) bool {
	if ValidJobTransition(current.Status, to) {
		return true
	}
	pending, _ := current.Result["cancel_pending"].(bool)
	return current.Status == domain.JobCanceled && pending && isTerminalJobStatus(to)
}

func latePendingCancellationUpdate(current domain.Job, to domain.JobStatus) bool {
	pending, _ := current.Result["cancel_pending"].(bool)
	return current.Status == domain.JobCanceled && pending && !isTerminalJobStatus(to)
}

// jobProjectionTarget returns immutable routing fields before the immediate
// projection transaction starts. An Agent replay can outlive an instance
// deletion, in which case instanceID is empty and there is no remaining
// instance state to project.
func (s *Store) jobProjectionTarget(ctx context.Context, id string) (jobType, instanceID string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT type,COALESCE(instance_id,'') FROM jobs WHERE id=?1`, id).Scan(&jobType, &instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return jobType, instanceID, err
}

func (s *Store) UpdateJob(ctx context.Context, id string, status domain.JobStatus, progress int, message string, result map[string]any) (domain.Job, error) {
	if progress < 0 || progress > 100 {
		return domain.Job{}, errors.New("progress must be between 0 and 100")
	}
	projectionType, projectionInstanceID := "", ""
	if isTerminalJobStatus(status) {
		var err error
		projectionType, projectionInstanceID, err = s.jobProjectionTarget(ctx, id)
		if err != nil {
			return domain.Job{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback()
	lockInstanceFirst := projectionInstanceID != "" && (projectionType == "restore" || status == domain.JobCanceled)
	instanceLocked := false
	if lockInstanceFirst {
		var lockedInstanceID string
		lockErr := tx.QueryRowContext(ctx, `SELECT id FROM instances WHERE id=?1`, projectionInstanceID).Scan(&lockedInstanceID)
		if errors.Is(lockErr, sql.ErrNoRows) {
			// A replay can outlive an instance that was removed after the original
			// projection and acknowledgement attempt. There is no remaining state
			// to project in that case.
		} else if lockErr != nil {
			return domain.Job{}, lockErr
		} else {
			instanceLocked = true
		}
	}
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, err
	}
	if projectionType != "" && current.Type != projectionType {
		return domain.Job{}, errors.New("job projection type changed while completing job")
	}
	if instanceLocked && current.InstanceID != projectionInstanceID {
		return domain.Job{}, errors.New("job projection target changed while completing job")
	}
	if latePendingCancellationUpdate(current, status) {
		return domain.Job{}, errLateCanceledJobUpdate
	}
	if settledTerminalJob(current) {
		if persistedTerminalOutcomeEquivalent(current, status, progress, message, result) {
			// The Agent is replaying an unacknowledged journal entry. Preserve the
			// original row byte-for-byte so randomized secret ciphertext and the
			// projection source of truth cannot drift.
			return current, nil
		}
		return domain.Job{}, errTerminalOutcomeConflict
	}
	if !validPersistedJobTransition(current, status) {
		return domain.Job{}, fmt.Errorf("invalid job transition %s -> %s", current.Status, status)
	}
	if progress < current.Progress {
		progress = current.Progress
	}
	result = cloneMap(result)
	// This field belongs exclusively to Control. A compromised or newer Agent
	// cannot suppress the durable projection barrier by supplying false.
	delete(result, jobProjectionPendingField)
	if current.Status == domain.JobCanceled {
		if pending, _ := current.Result["cancel_pending"].(bool); pending && isTerminalJobStatus(status) {
			// The Agent owns the final outcome after a dispatched task has begun.
			// Clearing this marker also releases the partial unique-index lock.
			delete(result, "cancel_pending")
		}
	}
	if requiresDeferredJobProjection(current.Type, status) {
		result[jobProjectionPendingField] = true
	}
	resultRaw, _ := json.Marshal(result)
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status=?2,progress=?3,message=?4,result=?5,
		started_at=CASE WHEN ?2='running' THEN COALESCE(started_at,unixepoch('subsec')*1000000) ELSE started_at END,
		finished_at=CASE WHEN ?2 IN ('succeeded','failed','canceled') THEN COALESCE(finished_at,unixepoch('subsec')*1000000) ELSE finished_at END
		WHERE id=?1`, id, status, progress, message, string(resultRaw))
	if err != nil {
		return domain.Job{}, err
	}
	current.Status = status
	current.Progress = progress
	current.Message = message
	current.Result = result
	if status == domain.JobCanceled {
		if err := applyJobCancellationTx(ctx, tx, current); err != nil {
			return domain.Job{}, err
		}
	} else if projectionType == "restore" && instanceLocked {
		if err := applyRestoreTerminalProjectionTx(ctx, tx, current); err != nil {
			return domain.Job{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Job{}, err
	}
	return s.GetJob(ctx, id)
}

func (s *Store) JobsForDispatch(ctx context.Context, nodeID string, limit int) ([]domain.Job, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	// Give ordinary work, one serialized automatic maintenance update, and the
	// durable cancellation outbox independent quotas. Stuck cancellation rows
	// cannot hide normal work, and a large manual backlog cannot starve a due
	// maintenance batch. One statement snapshot also prevents a concurrent
	// cancellation from placing the same job in two groups.
	rows, err := s.db.QueryContext(ctx, `WITH node_jobs AS (
		SELECT jobs.*,(jobs.type='update' AND jobs.requested_by IS NULL
			AND json_extract(jobs.payload,'$.maintenance')=1
			AND jobs.idempotency_key LIKE 'maintenance:update:%') AS automatic_maintenance
		FROM jobs WHERE jobs.node_id=?1 AND (
			jobs.status IN ('queued','dispatched','running')
			OR (jobs.status='canceled' AND json_extract(jobs.result,'$.cancel_pending')=1)
			OR (jobs.status IN ('succeeded','failed') AND json_extract(jobs.result,'$._projection_pending')=1)
		)
	), active_write_instances AS (
		SELECT DISTINCT job.instance_id FROM node_jobs job WHERE job.instance_id IS NOT NULL
			AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`
	), automatic_busy AS (
		SELECT EXISTS(SELECT 1 FROM node_jobs job WHERE job.automatic_maintenance AND (
			job.status IN ('dispatched','running')
			OR (job.status='canceled' AND json_extract(job.result,'$.cancel_pending')=1)
			OR (job.status IN ('succeeded','failed') AND json_extract(job.result,'$._projection_pending')=1)
		)) AS busy
	), automatic_candidate AS (
		SELECT job.id FROM node_jobs job,automatic_busy
		WHERE job.automatic_maintenance AND job.status IN ('queued','dispatched')
			AND (job.status='dispatched' OR NOT automatic_busy.busy)
		ORDER BY CASE WHEN job.status='dispatched' THEN 0 ELSE 1 END,job.created_at,job.id LIMIT 1
	), ordinary_candidates AS (
		SELECT job.id,job.created_at FROM node_jobs job
		WHERE NOT job.automatic_maintenance AND job.status IN ('queued','dispatched')
			AND NOT (job.type='check_update' AND job.instance_id IN (SELECT instance_id FROM active_write_instances))
		ORDER BY job.created_at,job.id LIMIT ?2
	), cancellation_candidates AS (
		SELECT job.id,job.created_at FROM node_jobs job
		WHERE job.status='canceled' AND json_extract(job.result,'$.cancel_pending')=1
		ORDER BY job.created_at,job.id LIMIT ?2
	), candidate_ids AS (
		SELECT id,0 AS dispatch_priority,created_at FROM ordinary_candidates
		UNION ALL
		SELECT job.id,0 AS dispatch_priority,job.created_at FROM node_jobs job
			WHERE job.id IN (SELECT id FROM automatic_candidate)
		UNION ALL
		SELECT id,1 AS dispatch_priority,created_at FROM cancellation_candidates
	), dispatch_jobs AS (
		SELECT jobs.*,candidate_ids.dispatch_priority FROM jobs JOIN candidate_ids USING(id)
	)
	SELECT `+jobColumns+` FROM dispatch_jobs ORDER BY dispatch_priority,created_at,id`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, job)
	}
	return items, rows.Err()
}

func (s *Store) MarkDispatched(ctx context.Context, id string) (bool, error) {
	preflight, err := s.GetJob(ctx, id)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if (isAutomaticMaintenanceUpdate(preflight) || preflight.Type == "check_update" || preflight.Type == "a2s.info") && preflight.InstanceID != "" {
		var lockedID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM instances WHERE id=?1`, preflight.InstanceID).Scan(&lockedID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrNotFound
			}
			return false, err
		}
	}
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if current.Status != domain.JobQueued {
		return false, tx.Commit()
	}
	if isAutomaticMaintenanceUpdate(current) != isAutomaticMaintenanceUpdate(preflight) || current.InstanceID != preflight.InstanceID || current.Type != preflight.Type {
		return false, errors.New("job dispatch identity changed while claiming")
	}
	if current.Type == "a2s.info" && current.RequestedBy == "" {
		var eligible bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM instances instance JOIN nodes node ON node.id=instance.node_id
			WHERE instance.id=?1 AND instance.node_id=?2 AND instance.status='running' AND node.status='online'
			AND NOT EXISTS (SELECT 1 FROM jobs job WHERE job.instance_id=instance.id
				AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`)
		)`, current.InstanceID, current.NodeID).Scan(&eligible); err != nil {
			return false, err
		}
		if !eligible {
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='canceled',progress=100,message='probe no longer applicable',
				finished_at=unixepoch('subsec')*1000000 WHERE id=?1 AND status='queued'`, current.ID); err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
	}
	if current.Type == "check_update" {
		var activeWrite bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs job WHERE job.instance_id=?1
			AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`)`, current.InstanceID).Scan(&activeWrite); err != nil {
			return false, err
		}
		if activeWrite {
			return false, tx.Commit()
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='dispatched' WHERE id=?1`, id); err != nil {
		return false, err
	}
	if isAutomaticMaintenanceUpdate(current) {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET status='updating',updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, current.InstanceID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) CancelJob(ctx context.Context, id string) (domain.Job, domain.JobStatus, error) {
	projectionType, projectionInstanceID, err := s.jobProjectionTarget(ctx, id)
	if err != nil {
		return domain.Job{}, "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, "", err
	}
	defer tx.Rollback()
	instanceLocked := false
	if projectionInstanceID != "" {
		var lockedInstanceID string
		lockErr := tx.QueryRowContext(ctx, `SELECT id FROM instances WHERE id=?1`, projectionInstanceID).Scan(&lockedInstanceID)
		if errors.Is(lockErr, sql.ErrNoRows) {
		} else if lockErr != nil {
			return domain.Job{}, "", lockErr
		} else {
			instanceLocked = true
		}
	}
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, "", ErrNotFound
	}
	if err != nil {
		return domain.Job{}, "", err
	}
	if current.Type != projectionType {
		return domain.Job{}, current.Status, errors.New("job projection type changed while canceling job")
	}
	if instanceLocked && current.InstanceID != projectionInstanceID {
		return domain.Job{}, current.Status, errors.New("job projection target changed while canceling job")
	}
	if current.Status == domain.JobCanceled {
		if pending, _ := current.Result["cancel_pending"].(bool); !pending {
			// An ordinary queued cancellation or an Agent-confirmed canceled
			// outcome is already terminal. A repeated HTTP request must not erase
			// its immutable result fingerprint.
			if err := tx.Commit(); err != nil {
				return domain.Job{}, current.Status, err
			}
			return current, current.Status, nil
		}
	}
	if !ValidJobTransition(current.Status, domain.JobCanceled) {
		return domain.Job{}, current.Status, ErrConflict
	}
	result := map[string]any{}
	cancelPending := current.Status != domain.JobQueued && current.Status != domain.JobCanceled
	if pending, _ := current.Result["cancel_pending"].(bool); pending {
		cancelPending = true
	}
	if cancelPending {
		// Keep the per-instance write lock until the Agent acknowledges the
		// cancellation with its own canceled update.
		result["cancel_pending"] = true
	}
	resultRaw, _ := json.Marshal(result)
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='canceled',message='canceled by operator',result=?2,
		payload=CASE WHEN type='backup.upload.chunk' THEN json_remove(payload,'$.content_base64_ciphertext') ELSE payload END,
		finished_at=CASE WHEN ?3 THEN finished_at ELSE COALESCE(finished_at,unixepoch('subsec')*1000000) END WHERE id=?1`, id, string(resultRaw), cancelPending); err != nil {
		return domain.Job{}, current.Status, err
	}
	if !cancelPending {
		projected := current
		projected.Status = domain.JobCanceled
		projected.Message = "canceled by operator"
		projected.Result = result
		if err := applyJobCancellationTx(ctx, tx, projected); err != nil {
			return domain.Job{}, current.Status, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Job{}, current.Status, err
	}
	updated, err := s.GetJob(ctx, id)
	return updated, current.Status, err
}
