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

const (
	maxBackupUploadSize = int64(20 << 30)
	backupUploadTTL     = 24 * time.Hour
)

const backupUploadColumns = `id,instance_id,name,platform,size,sha256,received_size,status,
	COALESCE(last_chunk_job_id,''),COALESCE(restore_job_id,''),expires_at,created_at,updated_at`

func scanBackupUpload(row scanner) (domain.BackupUpload, error) {
	var upload domain.BackupUpload
	var size, received int64
	err := row.Scan(&upload.ID, &upload.InstanceID, &upload.Name, &upload.Platform, &size, &upload.SHA256,
		&received, &upload.Status, &upload.LastChunkJobID, &upload.RestoreJobID, &upload.ExpiresAt,
		&upload.CreatedAt, &upload.UpdatedAt)
	if err == nil {
		upload.Size = uint64(size)
		upload.ReceivedSize = uint64(received)
	}
	return upload, err
}

func validateBackupUploadMetadata(name, platform string, size int64, sha string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 180 || strings.ContainsAny(name, "\x00\r\n/\\") || !strings.HasSuffix(strings.ToLower(name), ".zip") {
		return errors.New("backup name must be a safe .zip filename of at most 180 characters")
	}
	if platform != "windows/amd64" && platform != "linux/amd64" {
		return errors.New("platform must be windows/amd64 or linux/amd64")
	}
	if size <= 0 || size > maxBackupUploadSize {
		return fmt.Errorf("backup size must be between 1 and %d bytes", maxBackupUploadSize)
	}
	if sha == "" {
		return errors.New("backup SHA-256 is required")
	}
	return validateSHA256(sha)
}

func (s *Store) CreateBackupUpload(ctx context.Context, instanceID, name, platform string, size int64, sha, requestedBy, idempotencyKey string) (domain.BackupUpload, bool, error) {
	name, platform, sha = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(platform)), strings.ToLower(strings.TrimSpace(sha))
	if err := validateBackupUploadMetadata(name, platform, size, sha); err != nil {
		return domain.BackupUpload{}, false, err
	}
	if requestedBy == "" {
		return domain.BackupUpload{}, false, errors.New("authenticated requester is required")
	}
	if len(idempotencyKey) > 200 {
		return domain.BackupUpload{}, false, errors.New("idempotency key is too long")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.BackupUpload{}, false, err
	}
	defer tx.Rollback()
	if idempotencyKey != "" {
		existing, lookupErr := scanBackupUpload(tx.QueryRowContext(ctx, `SELECT `+backupUploadColumns+` FROM backup_uploads
			WHERE requested_by=?1 AND create_idempotency_key=?2`, requestedBy, idempotencyKey))
		if lookupErr == nil {
			if existing.InstanceID != instanceID || existing.Name != name || existing.Platform != platform ||
				int64(existing.Size) != size || !strings.EqualFold(existing.SHA256, sha) {
				return domain.BackupUpload{}, false, ErrConflict
			}
			return existing, true, tx.Commit()
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.BackupUpload{}, false, lookupErr
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM instances WHERE id=?1`, instanceID).Scan(&instanceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.BackupUpload{}, false, ErrNotFound
		}
		return domain.BackupUpload{}, false, err
	}
	now := time.Now().UTC()
	upload := domain.BackupUpload{ID: uuid.NewString(), InstanceID: instanceID, Name: name, Platform: platform,
		Size: uint64(size), SHA256: sha, Status: "uploading", ExpiresAt: now.Add(backupUploadTTL), CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO backup_uploads(id,instance_id,name,platform,size,sha256,requested_by,
		create_idempotency_key,expires_at,created_at,updated_at) VALUES(?1,?2,?3,?4,?5,?6,?7,NULLIF(?8,''),?9,?10,?10)`,
		upload.ID, upload.InstanceID, upload.Name, upload.Platform, size, upload.SHA256, requestedBy,
		idempotencyKey, upload.ExpiresAt, now)
	if err != nil {
		return domain.BackupUpload{}, false, err
	}
	return upload, false, tx.Commit()
}

func (s *Store) GetBackupUpload(ctx context.Context, instanceID, uploadID string) (domain.BackupUpload, error) {
	parsedUploadID, err := uuid.Parse(strings.TrimSpace(uploadID))
	if err != nil {
		return domain.BackupUpload{}, ErrNotFound
	}
	uploadID = parsedUploadID.String()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE backup_uploads SET status='expired',updated_at=?3
		WHERE id=?1 AND instance_id=?2 AND status IN ('uploading','ready') AND expires_at<=?3
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.type='backup.upload.chunk'
			AND json_extract(j.payload,'$.upload_id')=backup_uploads.id AND (
				j.status IN ('queued','dispatched','running') OR
				(j.status='succeeded' AND j.id IS NOT backup_uploads.last_chunk_job_id
					AND json_extract(j.payload,'$.offset')=backup_uploads.received_size)))`, uploadID, instanceID, now); err != nil {
		return domain.BackupUpload{}, err
	}
	upload, err := scanBackupUpload(s.db.QueryRowContext(ctx, `SELECT `+backupUploadColumns+` FROM backup_uploads WHERE id=?1 AND instance_id=?2`, uploadID, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.BackupUpload{}, ErrNotFound
	}
	return upload, err
}

func (s *Store) QueueBackupUploadChunk(ctx context.Context, instanceID, uploadID string, payload map[string]any, requestedBy, idempotencyKey string) (domain.Job, bool, error) {
	requestFingerprint, err := instanceJobRequestFingerprint(instanceID, "backup.upload.chunk", payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	return s.QueueBackupUploadChunkWithFingerprint(ctx, instanceID, uploadID, payload, requestedBy, idempotencyKey, requestFingerprint)
}

func (s *Store) QueueBackupUploadChunkWithFingerprint(ctx context.Context, instanceID, uploadID string, payload map[string]any, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	parsedUploadID, parseErr := uuid.Parse(strings.TrimSpace(uploadID))
	if parseErr != nil {
		return domain.Job{}, false, ErrNotFound
	}
	uploadID = parsedUploadID.String()
	if requestedBy == "" || strings.TrimSpace(idempotencyKey) == "" {
		return domain.Job{}, false, errors.New("Idempotency-Key is required for backup chunks")
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
	existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
	if lookupErr == nil {
		if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
			return domain.Job{}, false, ErrConflict
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return domain.Job{}, false, lookupErr
	}
	upload, err := scanBackupUpload(tx.QueryRowContext(ctx, `SELECT `+backupUploadColumns+` FROM backup_uploads WHERE id=?1 AND instance_id=?2`, uploadID, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if upload.Status != "uploading" || !time.Now().UTC().Before(upload.ExpiresAt) {
		return domain.Job{}, false, ErrConflict
	}
	offset, length := int64FromAny(payload["offset"]), intFromAny(payload["length"])
	if offset != int64(upload.ReceivedSize) || length < 1 || length > maxHTTPTransferChunk || offset+int64(length) > int64(upload.Size) {
		return domain.Job{}, false, ErrConflict
	}
	if int64FromAny(payload["expected_size"]) != int64(upload.Size) ||
		!strings.EqualFold(stringFromAny(payload["expected_sha256"]), upload.SHA256) ||
		stringFromAny(payload["chunk_sha256"]) == "" || validateSHA256(stringFromAny(payload["chunk_sha256"])) != nil {
		return domain.Job{}, false, errors.New("backup chunk metadata does not match the upload session")
	}
	if stringFromAny(payload["content_base64"]) != encryptedValuePlaceholder ||
		stringFromAny(payload["content_base64_ciphertext"]) == "" || len(stringFromAny(payload["content_base64_ciphertext"])) > 3<<20 {
		return domain.Job{}, false, errors.New("backup chunk body must be sealed with the deployment master key")
	}
	// UpdateJob and applyBackupUploadChunkResult are deliberately separate so a
	// terminal result can be replayed after a projection failure. During that
	// window received_size has not advanced yet; coalesce the same chunk and
	// reject conflicting bytes instead of scheduling a second Agent write.
	unprojected, unprojectedErr := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE instance_id=?1 AND type='backup.upload.chunk' AND status='succeeded'
		AND json_extract(payload,'$.upload_id')=?2 AND json_extract(payload,'$.offset')=?3
		AND id IS NOT NULLIF(?4,'')
		ORDER BY finished_at DESC,id DESC LIMIT 1`, instanceID, uploadID, offset, upload.LastChunkJobID))
	if unprojectedErr == nil {
		if !sameBackupUploadChunk(unprojected, instanceID, uploadID, payload) {
			return domain.Job{}, false, ErrConflict
		}
		return unprojected, true, tx.Commit()
	}
	if !errors.Is(unprojectedErr, sql.ErrNoRows) {
		return domain.Job{}, false, unprojectedErr
	}
	var nodeID string
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM instances WHERE id=?1`, instanceID).Scan(&nodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID, Type: "backup.upload.chunk",
		Status: domain.JobQueued, Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)`, job.ID, job.NodeID, job.InstanceID, job.Type, job.Status,
		string(raw), job.IdempotencyKey, job.RequestedBy, job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}

func sameBackupUploadChunk(job domain.Job, instanceID, uploadID string, payload map[string]any) bool {
	return job.Type == "backup.upload.chunk" && job.InstanceID == instanceID &&
		stringFromAny(job.Payload["upload_id"]) == uploadID &&
		int64FromAny(job.Payload["offset"]) == int64FromAny(payload["offset"]) &&
		intFromAny(job.Payload["length"]) == intFromAny(payload["length"]) &&
		strings.EqualFold(stringFromAny(job.Payload["chunk_sha256"]), stringFromAny(payload["chunk_sha256"]))
}

func (s *Store) QueueBackupUploadRestore(ctx context.Context, instanceID, uploadID string, payload map[string]any, requestedBy, idempotencyKey string) (domain.Job, bool, error) {
	requestFingerprint, err := instanceJobRequestFingerprint(instanceID, "restore", payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	return s.QueueBackupUploadRestoreWithFingerprint(ctx, instanceID, uploadID, payload, requestedBy, idempotencyKey, requestFingerprint)
}

func (s *Store) QueueBackupUploadRestoreWithFingerprint(ctx context.Context, instanceID, uploadID string, payload map[string]any, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	parsedUploadID, parseErr := uuid.Parse(strings.TrimSpace(uploadID))
	if parseErr != nil {
		return domain.Job{}, false, ErrNotFound
	}
	uploadID = parsedUploadID.String()
	if requestedBy == "" {
		return domain.Job{}, false, errors.New("authenticated requester is required")
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
		existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
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
	upload, err := scanBackupUpload(tx.QueryRowContext(ctx, `SELECT `+backupUploadColumns+` FROM backup_uploads WHERE id=?1 AND instance_id=?2`, uploadID, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if upload.RestoreJobID != "" {
		existing, jobErr := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, upload.RestoreJobID))
		if jobErr != nil {
			return domain.Job{}, false, jobErr
		}
		switch existing.Status {
		case domain.JobQueued, domain.JobDispatched, domain.JobRunning, domain.JobSucceeded:
			return existing, true, tx.Commit()
		case domain.JobFailed, domain.JobCanceled:
			// The Agent rolls a failed restore back to its checkpoint and keeps the
			// verified upload. The failed upload status is the projection barrier:
			// before it is visible, a new key must not overwrite restore_job_id.
			if upload.Status != "failed" {
				return domain.Job{}, false, ErrConflict
			}
			// A new Idempotency-Key may now retry without uploading the same
			// archive again.
			upload.Status = "ready"
			upload.RestoreJobID = ""
		default:
			return domain.Job{}, false, ErrConflict
		}
	}
	if upload.Status != "ready" || upload.ReceivedSize != upload.Size || !time.Now().UTC().Before(upload.ExpiresAt) {
		return domain.Job{}, false, ErrConflict
	}
	if int64FromAny(payload["expected_size"]) != int64(upload.Size) ||
		!strings.EqualFold(stringFromAny(payload["expected_sha256"]), upload.SHA256) ||
		stringFromAny(payload["upload_id"]) != upload.ID {
		return domain.Job{}, false, errors.New("restore metadata does not match the upload session")
	}
	var nodeID string
	var instanceStatus domain.InstanceStatus
	if err := tx.QueryRowContext(ctx, `SELECT node_id,status FROM instances WHERE id=?1`, instanceID).Scan(&nodeID, &instanceStatus); err != nil {
		return domain.Job{}, false, err
	}
	if instanceStatus != domain.InstanceStopped && instanceStatus != domain.InstanceDegraded {
		return domain.Job{}, false, ErrConflict
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID, Type: "restore", Status: domain.JobQueued,
		Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,?4,?5,?6,NULLIF(?7,''),?8,?9,NULLIF(?10,''))`, job.ID, job.NodeID, job.InstanceID, job.Type,
		job.Status, string(raw), job.IdempotencyKey, job.RequestedBy, job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE backup_uploads SET status='restoring',restore_job_id=?2,updated_at=?3 WHERE id=?1`, upload.ID, job.ID, time.Now().UTC()); err != nil {
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}

func (s *Store) applyBackupUploadChunkResult(ctx context.Context, job domain.Job) error {
	uploadID := stringFromAny(job.Payload["upload_id"])
	offset, length := int64FromAny(job.Payload["offset"]), intFromAny(job.Payload["length"])
	next := offset + int64(length)
	if uploadID == "" || offset < 0 || length < 1 || length > maxHTTPTransferChunk ||
		stringFromAny(job.Result["upload_id"]) != uploadID || int64FromAny(job.Result["offset"]) != offset ||
		intFromAny(job.Result["length"]) != length || int64FromAny(job.Result["received_size"]) != next ||
		!strings.EqualFold(stringFromAny(job.Result["chunk_sha256"]), stringFromAny(job.Payload["chunk_sha256"])) {
		return errors.New("Agent returned invalid backup upload chunk metadata")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upload, err := scanBackupUpload(tx.QueryRowContext(ctx, `SELECT `+backupUploadColumns+` FROM backup_uploads WHERE id=?1 AND instance_id=?2`, uploadID, job.InstanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if upload.LastChunkJobID == job.ID && int64(upload.ReceivedSize) == next {
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET payload=json_remove(payload,'$.content_base64_ciphertext') WHERE id=?1`, job.ID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if upload.Status != "uploading" || offset != int64(upload.ReceivedSize) || next > int64(upload.Size) {
		return ErrConflict
	}
	complete := next == int64(upload.Size)
	if boolFromAny(job.Result["complete"]) != complete {
		return errors.New("Agent returned an invalid backup upload completion flag")
	}
	if complete && !strings.EqualFold(stringFromAny(job.Result["file_sha256"]), upload.SHA256) {
		return errors.New("Agent returned an invalid completed backup checksum")
	}
	status := "uploading"
	if complete {
		status = "ready"
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE backup_uploads SET received_size=?2,status=?3,last_chunk_job_id=?4,
		expires_at=?5,updated_at=?6 WHERE id=?1`, upload.ID, next, status, job.ID, now.Add(24*time.Hour), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET payload=json_remove(payload,'$.content_base64_ciphertext') WHERE id=?1`, job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// finishBackupUploadRestoreTx projects the public upload state inside the
// caller's transaction. Restore job completion uses this form so the active
// per-instance job lock is not released before the upload becomes retryable or
// terminal.
func finishBackupUploadRestoreTx(ctx context.Context, tx *sql.Tx, job domain.Job, status string) error {
	uploadID := stringFromAny(job.Payload["upload_id"])
	if uploadID == "" {
		return nil
	}
	if status != "succeeded" && status != "failed" {
		return errors.New("invalid backup upload restore status")
	}
	now := time.Now().UTC()
	tag, err := tx.ExecContext(ctx, `UPDATE backup_uploads SET status=?3,
		expires_at=CASE WHEN ?3='failed' THEN max(expires_at,?5) ELSE expires_at END,
		updated_at=?6
		WHERE id=?1 AND instance_id=?2 AND restore_job_id=?4 AND status='restoring'`, uploadID, job.InstanceID, status, job.ID, now.Add(24*time.Hour), now)
	if err != nil || resultRowsAffected(tag) > 0 {
		return err
	}
	var storedStatus, restoreJobID string
	if err := tx.QueryRowContext(ctx, `SELECT status,COALESCE(restore_job_id,'') FROM backup_uploads
		WHERE id=?1 AND instance_id=?2`, uploadID, job.InstanceID).Scan(&storedStatus, &restoreJobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if restoreJobID == job.ID && storedStatus == status {
		return nil
	}
	return ErrConflict
}

func (s *Store) scrubBackupUploadChunkBody(ctx context.Context, job domain.Job) error {
	if job.Type != "backup.upload.chunk" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET payload=json_remove(payload,'$.content_base64_ciphertext') WHERE id=?1`, job.ID)
	return err
}
