package control

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const maxTerminalOutcomeConflictsPerJob = 8

type TerminalOutcomeConflict struct {
	JobID               string           `json:"job_id"`
	NodeID              string           `json:"node_id"`
	InstanceID          string           `json:"instance_id,omitempty"`
	StoredStatus        domain.JobStatus `json:"stored_status"`
	ReportedStatus      domain.JobStatus `json:"reported_status"`
	ReportedFingerprint string           `json:"reported_fingerprint"`
	Reason              string           `json:"reason"`
	Occurrences         int              `json:"occurrences"`
	FirstSeenAt         time.Time        `json:"first_seen_at"`
	LastSeenAt          time.Time        `json:"last_seen_at"`
	Resolution          string           `json:"resolution,omitempty"`
	ResolvedAt          *time.Time       `json:"resolved_at,omitempty"`
	ResolvedBy          string           `json:"resolved_by,omitempty"`
	ResolutionKey       string           `json:"-"`
	ResolutionRequest   string           `json:"-"`
}

const terminalOutcomeConflictColumns = `job_id,node_id,instance_id,stored_status,reported_status,reported_fingerprint,
	reason,occurrences,first_seen_at,last_seen_at,COALESCE(resolution,''),resolved_at,COALESCE(resolved_by,''),
	COALESCE(resolution_idempotency_key,''),COALESCE(resolution_request_fingerprint,'')`

func scanTerminalOutcomeConflict(row scanner) (TerminalOutcomeConflict, error) {
	var item TerminalOutcomeConflict
	err := row.Scan(&item.JobID, &item.NodeID, &item.InstanceID, &item.StoredStatus, &item.ReportedStatus,
		&item.ReportedFingerprint, &item.Reason, &item.Occurrences, &item.FirstSeenAt, &item.LastSeenAt,
		&item.Resolution, &item.ResolvedAt, &item.ResolvedBy, &item.ResolutionKey, &item.ResolutionRequest)
	return item, err
}

func (s *Store) RecordTerminalOutcomeConflict(ctx context.Context, job domain.Job, reportedStatus domain.JobStatus, fingerprint, reason string) (TerminalOutcomeConflict, bool, error) {
	if !settledTerminalJob(job) || !isTerminalJobStatus(reportedStatus) || !validRequestFingerprint(fingerprint) {
		return TerminalOutcomeConflict{}, false, errors.New("invalid terminal outcome conflict")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 64 {
		return TerminalOutcomeConflict{}, false, errors.New("invalid terminal outcome conflict reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TerminalOutcomeConflict{}, false, err
	}
	defer tx.Rollback()

	item, lookupErr := scanTerminalOutcomeConflict(tx.QueryRowContext(ctx, `SELECT `+terminalOutcomeConflictColumns+`
		FROM job_terminal_conflicts WHERE job_id=?1 AND reported_fingerprint=?2`, job.ID, fingerprint))
	now := time.Now().UTC()
	if lookupErr == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE job_terminal_conflicts
			SET occurrences=occurrences+1,last_seen_at=?3
			WHERE job_id=?1 AND reported_fingerprint=?2`, job.ID, fingerprint, now); err != nil {
			return TerminalOutcomeConflict{}, false, err
		}
		item.Occurrences++
		item.LastSeenAt = now
		return item, false, tx.Commit()
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return TerminalOutcomeConflict{}, false, lookupErr
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM job_terminal_conflicts WHERE job_id=?1`, job.ID).Scan(&count); err != nil {
		return TerminalOutcomeConflict{}, false, err
	}
	if count >= maxTerminalOutcomeConflictsPerJob {
		// A compromised node cannot grow SQLite without bound by varying the
		// reported payload. The connection still treats the frame as a semantic
		// conflict, but only the first bounded set is operator-resolvable.
		return TerminalOutcomeConflict{JobID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID,
			StoredStatus: job.Status, ReportedStatus: reportedStatus, ReportedFingerprint: fingerprint,
			Reason: reason, Occurrences: 1, FirstSeenAt: now, LastSeenAt: now}, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_terminal_conflicts(
		job_id,reported_fingerprint,node_id,instance_id,stored_status,reported_status,reason,occurrences,first_seen_at,last_seen_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7,1,?8,?8)`, job.ID, fingerprint, job.NodeID, job.InstanceID,
		job.Status, reportedStatus, reason, now); err != nil {
		return TerminalOutcomeConflict{}, false, err
	}
	item = TerminalOutcomeConflict{JobID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID,
		StoredStatus: job.Status, ReportedStatus: reportedStatus, ReportedFingerprint: fingerprint,
		Reason: reason, Occurrences: 1, FirstSeenAt: now, LastSeenAt: now}
	return item, true, tx.Commit()
}

func (s *Store) ListTerminalOutcomeConflicts(ctx context.Context, activeOnly bool, limit int) ([]TerminalOutcomeConflict, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+terminalOutcomeConflictColumns+` FROM job_terminal_conflicts
		WHERE (?1=0 OR resolution IS NULL)
		ORDER BY CASE WHEN resolution IS NULL THEN 0 ELSE 1 END,last_seen_at DESC,job_id,reported_fingerprint LIMIT ?2`, activeOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TerminalOutcomeConflict, 0)
	for rows.Next() {
		item, err := scanTerminalOutcomeConflict(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetTerminalOutcomeConflict(ctx context.Context, jobID, fingerprint string) (TerminalOutcomeConflict, error) {
	query := `SELECT ` + terminalOutcomeConflictColumns + ` FROM job_terminal_conflicts WHERE job_id=?1`
	args := []any{jobID}
	if fingerprint != "" {
		query += ` AND reported_fingerprint=?2`
		args = append(args, fingerprint)
	}
	query += ` ORDER BY CASE WHEN resolution IS NULL THEN 0 ELSE 1 END,last_seen_at DESC LIMIT 1`
	item, err := scanTerminalOutcomeConflict(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return TerminalOutcomeConflict{}, ErrNotFound
	}
	return item, err
}

func (s *Store) ResolveTerminalOutcomeConflict(ctx context.Context, jobID, fingerprint, actorID, idempotencyKey, requestFingerprint string) (TerminalOutcomeConflict, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 200 || !validRequestFingerprint(fingerprint) ||
		!validRequestFingerprint(requestFingerprint) || actorID == "" {
		return TerminalOutcomeConflict{}, false, errors.New("invalid terminal outcome conflict resolution")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TerminalOutcomeConflict{}, false, err
	}
	defer tx.Rollback()
	item, err := scanTerminalOutcomeConflict(tx.QueryRowContext(ctx, `SELECT `+terminalOutcomeConflictColumns+`
		FROM job_terminal_conflicts WHERE job_id=?1 AND reported_fingerprint=?2`, jobID, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return TerminalOutcomeConflict{}, false, ErrNotFound
	}
	if err != nil {
		return TerminalOutcomeConflict{}, false, err
	}
	if item.Resolution != "" {
		if item.Resolution == "keep_control" && item.ResolutionKey == idempotencyKey &&
			requestFingerprintsEqual(item.ResolutionRequest, requestFingerprint) {
			return item, true, tx.Commit()
		}
		return TerminalOutcomeConflict{}, false, ErrConflict
	}
	resolvedAt := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE job_terminal_conflicts SET resolution='keep_control',resolved_at=?3,
		resolved_by=?4,resolution_idempotency_key=?5,resolution_request_fingerprint=?6
		WHERE job_id=?1 AND reported_fingerprint=?2 AND resolution IS NULL`, jobID, fingerprint, resolvedAt,
		actorID, idempotencyKey, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return TerminalOutcomeConflict{}, false, ErrConflict
		}
		return TerminalOutcomeConflict{}, false, err
	}
	item.Resolution, item.ResolvedAt, item.ResolvedBy = "keep_control", &resolvedAt, actorID
	item.ResolutionKey, item.ResolutionRequest = idempotencyKey, requestFingerprint
	return item, false, tx.Commit()
}
