package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// QueueBackupUploadAbort expires an upload session and durably asks the target
// Agent to remove its staging files. The immediate SQLite transaction is
// shared with chunk and restore creation, so no new writer can slip between
// the safety checks and the status change.
func (s *Store) QueueBackupUploadAbort(ctx context.Context, instanceID, uploadID, requestedBy, idempotencyKey string) (domain.Job, bool, error) {
	payload := map[string]any{"upload_id": strings.TrimSpace(uploadID)}
	requestFingerprint, err := instanceJobRequestFingerprint(instanceID, "backup.upload.abort", payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	return s.QueueBackupUploadAbortWithFingerprint(ctx, instanceID, uploadID, requestedBy, idempotencyKey, requestFingerprint)
}

func (s *Store) QueueBackupUploadAbortWithFingerprint(ctx context.Context, instanceID, uploadID, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	parsedUploadID, err := uuid.Parse(strings.TrimSpace(uploadID))
	if err != nil {
		return domain.Job{}, false, ErrNotFound
	}
	uploadID = parsedUploadID.String()
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if strings.TrimSpace(requestedBy) == "" {
		return domain.Job{}, false, errors.New("authenticated requester is required")
	}
	if idempotencyKey == "" {
		return domain.Job{}, false, errors.New("Idempotency-Key is required to abandon a backup upload")
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

	// Check the global per-requester key before looking at mutable session
	// state. A retry therefore still returns its original abort job after the
	// first request atomically changed the upload to expired.
	existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+`
		FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
	if lookupErr == nil {
		if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
			return domain.Job{}, false, ErrConflict
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return domain.Job{}, false, lookupErr
	}

	upload, err := scanBackupUpload(tx.QueryRowContext(ctx, `SELECT `+backupUploadColumns+`
		FROM backup_uploads WHERE id=?1 AND instance_id=?2`, uploadID, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if upload.Status != "uploading" && upload.Status != "ready" && upload.Status != "failed" {
		return domain.Job{}, false, ErrConflict
	}

	// A canceled-but-unacknowledged job remains Agent-owned. A succeeded chunk
	// with a durable projection barrier is still capable of changing the public
	// session when its result is replayed. Neither may race staging deletion.
	var conflicting bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM jobs j
		WHERE j.instance_id=?1 AND j.type IN ('backup.upload.chunk','restore')
		  AND json_extract(j.payload,'$.upload_id')=?2
		  AND (
			j.status IN ('queued','dispatched','running')
			OR (j.status='canceled' AND json_extract(j.result,'$.cancel_pending')=1)
			OR (j.type='backup.upload.chunk' AND j.status='succeeded'
				AND json_extract(j.result,'$._projection_pending')=1)
		  )
	)`, instanceID, uploadID).Scan(&conflicting); err != nil {
		return domain.Job{}, false, err
	}
	if conflicting {
		return domain.Job{}, false, ErrConflict
	}

	// restore_job_id is the projection barrier for terminal restores. Older
	// failed attempts may remain in job history; only the current pointer needs
	// to agree with the upload status.
	if upload.RestoreJobID != "" {
		var restoreStatus domain.JobStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=?1 AND instance_id=?2 AND type='restore'`, upload.RestoreJobID, instanceID).Scan(&restoreStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.Job{}, false, ErrConflict
			}
			return domain.Job{}, false, err
		}
		if (restoreStatus != domain.JobFailed && restoreStatus != domain.JobCanceled) || upload.Status != "failed" {
			return domain.Job{}, false, ErrConflict
		}
	}

	var nodeID string
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM instances WHERE id=?1`, instanceID).Scan(&nodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	payload := map[string]any{"upload_id": uploadID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	now := time.Now().UTC()
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID, Type: "backup.upload.abort",
		Status: domain.JobQueued, Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)`, job.ID, job.NodeID, job.InstanceID, job.Type, job.Status,
		string(raw), job.IdempotencyKey, job.RequestedBy, job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE backup_uploads SET status='expired',expires_at=min(expires_at,?2),updated_at=?2
		WHERE id=?1`, uploadID, now); err != nil {
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}
