package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	quarantineRetention       = 7 * 24 * time.Hour
	purgeConfirmationTokenTTL = 5 * time.Minute
)

var controlQuarantineEntryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+-(\d{8}T\d{6}Z)$`)

const quarantineEntryColumns = `id,node_id,source_job_id,original_instance_id,instance_name,root_index,entry_name,
	quarantined_at,retention_expires_at,COALESCE(purge_job_id,''),purged_at,created_at`

func scanQuarantineEntry(row scanner) (domain.QuarantineEntry, error) {
	var entry domain.QuarantineEntry
	err := row.Scan(&entry.ID, &entry.NodeID, &entry.SourceJobID, &entry.OriginalInstanceID, &entry.InstanceName,
		&entry.RootIndex, &entry.EntryName, &entry.QuarantinedAt, &entry.RetentionExpiresAt, &entry.PurgeJobID,
		&entry.PurgedAt, &entry.CreatedAt)
	return entry, err
}

func parseQuarantineEntryTime(entryName string) (time.Time, error) {
	if len(entryName) == 0 || len(entryName) > 200 || strings.ContainsAny(entryName, `/\`) {
		return time.Time{}, errors.New("invalid quarantine entry name")
	}
	matches := controlQuarantineEntryPattern.FindStringSubmatch(entryName)
	if len(matches) != 2 {
		return time.Time{}, errors.New("invalid quarantine entry name")
	}
	value, err := time.Parse("20060102T150405Z", matches[1])
	if err != nil {
		return time.Time{}, errors.New("invalid quarantine entry timestamp")
	}
	return value.UTC(), nil
}

// ApplyInstanceRemovalResult records the safe quarantine basename before the
// instance row is deleted. It also strips legacy absolute quarantine paths from
// the persisted job result.
func (s *Store) ApplyInstanceRemovalResult(ctx context.Context, job domain.Job) error {
	mode := strings.ToLower(strings.TrimSpace(stringFromAny(job.Payload["mode"])))
	if job.Type == "detach" {
		mode = "detach"
	}
	if mode == "" {
		mode = "quarantine"
	}
	if mode != "quarantine" && mode != "detach" {
		return errors.New("invalid instance removal mode")
	}
	if job.InstanceID == "" {
		// Terminal replay after the original instance delete has already nulled
		// jobs.instance_id.
		return nil
	}

	result := cloneMap(job.Result)
	delete(result, "quarantine_path")
	entryName := strings.TrimSpace(stringFromAny(result["quarantine_entry"]))
	rootIndex, hasRootIndex := nonNegativeQuarantineRootIndex(result["root_index"])
	var entryTimestamp time.Time
	var err error
	if mode == "quarantine" {
		if entryName == "" {
			return errors.New("Agent quarantine result is missing the safe entry basename")
		}
		entryTimestamp, err = parseQuarantineEntryTime(entryName)
		if err != nil {
			return err
		}
		if !hasRootIndex {
			return errors.New("invalid quarantine root index")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var instanceName, nodeID string
	err = tx.QueryRowContext(ctx, `SELECT name,node_id FROM instances WHERE id=?1`, job.InstanceID).Scan(&instanceName, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		var existingID string
		if lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM quarantine_entries WHERE source_job_id=?1`, job.ID).Scan(&existingID); lookupErr == nil {
			return tx.Commit()
		}
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if nodeID != job.NodeID {
		return ErrUnauthorized
	}

	if mode == "quarantine" {
		entryID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("l4d2panel:quarantine:"+job.ID)).String()
		// Begin the minimum-retention clock no earlier than the Control plane
		// receives the result. A stale journal replay or a forged old basename
		// can therefore extend retention but can never shorten it.
		quarantinedAt := time.Now().UTC().Truncate(time.Second)
		if entryTimestamp.After(quarantinedAt) {
			quarantinedAt = entryTimestamp
		}
		retentionExpiresAt := quarantinedAt.Add(quarantineRetention)
		if _, err := tx.ExecContext(ctx, `INSERT INTO quarantine_entries(
			id,node_id,source_job_id,original_instance_id,instance_name,root_index,entry_name,quarantined_at,retention_expires_at)
			VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9) ON CONFLICT(source_job_id) DO NOTHING`, entryID, nodeID, job.ID,
			job.InstanceID, instanceName, rootIndex, entryName, quarantinedAt, retentionExpiresAt); err != nil {
			return err
		}
		result["quarantine_id"] = entryID
		result["retention_expires_at"] = retentionExpiresAt.Format(time.RFC3339)
	}
	resultRaw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET result=?2 WHERE id=?1`, job.ID, string(resultRaw)); err != nil {
		return err
	}
	if tag, err := tx.ExecContext(ctx, `DELETE FROM instances WHERE id=?1`, job.InstanceID); err != nil {
		return err
	} else if resultRowsAffected(tag) != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func nonNegativeQuarantineRootIndex(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, typed >= 0
	case int32:
		return int(typed), typed >= 0
	case int64:
		return int(typed), typed >= 0 && int64(int(typed)) == typed
	case float64:
		converted := int(typed)
		return converted, typed >= 0 && float64(converted) == typed
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil && parsed >= 0 && int64(int(parsed)) == parsed
	default:
		return 0, false
	}
}

func (s *Store) ListQuarantineEntries(ctx context.Context, nodeID string) ([]domain.QuarantineEntry, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=?1)`, nodeID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+quarantineEntryColumns+` FROM quarantine_entries
		WHERE node_id=?1 AND purged_at IS NULL ORDER BY quarantined_at DESC,id DESC`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.QuarantineEntry, 0)
	for rows.Next() {
		item, err := scanQuarantineEntry(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CreateQuarantinePurgeConfirmation(ctx context.Context, nodeID, entryID, requestedBy string) (string, time.Time, error) {
	if uuid.Validate(nodeID) != nil || uuid.Validate(entryID) != nil || uuid.Validate(requestedBy) != nil {
		return "", time.Time{}, ErrNotFound
	}
	entry, err := scanQuarantineEntry(s.db.QueryRowContext(ctx, `SELECT `+quarantineEntryColumns+` FROM quarantine_entries
		WHERE id=?1 AND node_id=?2 AND purged_at IS NULL`, entryID, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if entry.RetentionExpiresAt.After(time.Now().UTC()) {
		return "", time.Time{}, fmt.Errorf("quarantine entry is still within its seven-day minimum retention: %w", ErrConflict)
	}
	random, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	raw := "qpc_" + random
	now := time.Now().UTC()
	expiresAt := now.Add(purgeConfirmationTokenTTL)
	_, err = s.db.ExecContext(ctx, `INSERT INTO quarantine_purge_confirmations(
		id,quarantine_entry_id,requested_by,token_hash,expires_at,created_at) VALUES(?1,?2,?3,?4,?5,?6)`,
		uuid.NewString(), entryID, requestedBy, tokenHash(raw), expiresAt, now)
	return raw, expiresAt, err
}

func (s *Store) QueueQuarantinePurge(ctx context.Context, nodeID, entryID, requestedBy, confirmationToken, idempotencyKey string) (domain.Job, bool, error) {
	requestFingerprint, err := canonicalRequestFingerprint("quarantine", nodeID, "purge", map[string]any{
		"entry_id": entryID, "confirmation_token": confirmationToken,
	})
	if err != nil {
		return domain.Job{}, false, err
	}
	return s.QueueQuarantinePurgeWithFingerprint(ctx, nodeID, entryID, requestedBy, confirmationToken, idempotencyKey, requestFingerprint)
}

func (s *Store) QueueQuarantinePurgeWithFingerprint(ctx context.Context, nodeID, entryID, requestedBy, confirmationToken, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if len(idempotencyKey) > 200 {
		return domain.Job{}, false, errors.New("idempotency key is too long")
	}
	if !strings.HasPrefix(confirmationToken, "qpc_") || len(confirmationToken) > 128 {
		return domain.Job{}, false, ErrUnauthorized
	}
	if err := requireRequestFingerprint(idempotencyKey, requestFingerprint); err != nil {
		return domain.Job{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback()

	var confirmationID, confirmedEntryID, confirmedBy, confirmationJobID string
	var expiresAt time.Time
	var consumedAt *time.Time
	err = tx.QueryRowContext(ctx, `SELECT id,quarantine_entry_id,requested_by,expires_at,consumed_at,COALESCE(job_id,'')
		FROM quarantine_purge_confirmations WHERE token_hash=?1`, tokenHash(confirmationToken)).Scan(
		&confirmationID, &confirmedEntryID, &confirmedBy, &expiresAt, &consumedAt, &confirmationJobID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, ErrUnauthorized
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if confirmedEntryID != entryID || confirmedBy != requestedBy || !expiresAt.After(time.Now().UTC()) {
		return domain.Job{}, false, ErrUnauthorized
	}
	if consumedAt != nil {
		if idempotencyKey != "" && confirmationJobID != "" {
			existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE id=?1`, confirmationJobID))
			if lookupErr == nil && existing.IdempotencyKey == idempotencyKey && requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
				return existing, true, tx.Commit()
			}
		}
		return domain.Job{}, false, fmt.Errorf("purge confirmation has already been consumed: %w", ErrConflict)
	}

	entry, err := scanQuarantineEntry(tx.QueryRowContext(ctx, `SELECT `+quarantineEntryColumns+` FROM quarantine_entries WHERE id=?1`, entryID))
	if errors.Is(err, sql.ErrNoRows) || entry.NodeID != nodeID || entry.PurgedAt != nil {
		return domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if entry.RetentionExpiresAt.After(time.Now().UTC()) {
		return domain.Job{}, false, fmt.Errorf("quarantine entry is still within its seven-day minimum retention: %w", ErrConflict)
	}
	if idempotencyKey != "" {
		existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
		if lookupErr == nil {
			if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
				return domain.Job{}, false, ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE quarantine_entries SET purge_job_id=?2 WHERE id=?1`, entry.ID, existing.ID); err != nil {
				return domain.Job{}, false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE quarantine_purge_confirmations SET consumed_at=unixepoch('subsec')*1000000,job_id=?2 WHERE id=?1`, confirmationID, existing.ID); err != nil {
				return domain.Job{}, false, err
			}
			return existing, true, tx.Commit()
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.Job{}, false, lookupErr
		}
	}
	if entry.PurgeJobID != "" {
		existing, lookupErr := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, entry.PurgeJobID))
		if lookupErr == nil && (existing.Status == domain.JobQueued || existing.Status == domain.JobDispatched || existing.Status == domain.JobRunning) {
			if idempotencyKey != "" {
				// A second key cannot be durably associated with the existing job.
				// Fail instead of consuming a confirmation whose retry could not be
				// proven equivalent.
				return domain.Job{}, false, ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE quarantine_purge_confirmations SET consumed_at=unixepoch('subsec')*1000000,job_id=?2 WHERE id=?1`, confirmationID, existing.ID); err != nil {
				return domain.Job{}, false, err
			}
			return existing, true, tx.Commit()
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.Job{}, false, lookupErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE quarantine_entries SET purge_job_id=NULL WHERE id=?1`, entry.ID); err != nil {
			return domain.Job{}, false, err
		}
	}

	var nodeStatus domain.NodeStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id=?1`, nodeID).Scan(&nodeStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if nodeStatus == domain.NodeRevoked {
		return domain.Job{}, false, ErrUnauthorized
	}
	payload := map[string]any{
		"quarantine_entry_id": entry.ID,
		"quarantine_entry":    entry.EntryName,
		"root_index":          entry.RootIndex,
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	job := domain.Job{
		ID: uuid.NewString(), NodeID: nodeID, Type: "purge_quarantine", Status: domain.JobQueued, Payload: payload,
		IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,NULL,?3,?4,?5,NULLIF(?6,''),?7,?8,NULLIF(?9,''))`, job.ID, job.NodeID, job.Type, job.Status, string(payloadRaw),
		job.IdempotencyKey, job.RequestedBy, job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE quarantine_entries SET purge_job_id=?2 WHERE id=?1`, entry.ID, job.ID); err != nil {
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE quarantine_purge_confirmations SET consumed_at=unixepoch('subsec')*1000000,job_id=?2 WHERE id=?1`, confirmationID, job.ID); err != nil {
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}

func (s *Store) ApplyQuarantinePurgeResult(ctx context.Context, job domain.Job) error {
	entryID := stringFromAny(job.Payload["quarantine_entry_id"])
	if uuid.Validate(entryID) != nil {
		return errors.New("purge job has an invalid quarantine entry ID")
	}
	tag, err := s.db.ExecContext(ctx, `UPDATE quarantine_entries SET purged_at=COALESCE(purged_at,unixepoch('subsec')*1000000),purge_job_id=?2
		WHERE id=?1 AND node_id=?3 AND (purge_job_id=?2 OR purged_at IS NOT NULL)`, entryID, job.ID, job.NodeID)
	if err != nil {
		return err
	}
	if resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReleaseQuarantinePurgeJob(ctx context.Context, job domain.Job) error {
	entryID := stringFromAny(job.Payload["quarantine_entry_id"])
	if uuid.Validate(entryID) != nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE quarantine_entries SET purge_job_id=NULL WHERE id=?1 AND node_id=?2 AND purge_job_id=?3 AND purged_at IS NULL`,
		entryID, job.NodeID, job.ID)
	return err
}

func releaseQuarantinePurgeJobTx(ctx context.Context, tx *sql.Tx, job domain.Job) error {
	entryID := stringFromAny(job.Payload["quarantine_entry_id"])
	if uuid.Validate(entryID) != nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE quarantine_entries SET purge_job_id=NULL
		WHERE id=?1 AND node_id=?2 AND purge_job_id=?3 AND purged_at IS NULL`, entryID, job.NodeID, job.ID)
	return err
}
