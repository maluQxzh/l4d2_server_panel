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

const mapDeleteConfirmationTTL = 5 * time.Minute

const mapColumns = `id,node_id,name,author,version,description,CASE WHEN length(poster_data)>0 THEN 1 ELSE 0 END,health,aggregate_sha256,total_size,source_type,source_name,
	removed_files,removed_bytes,COALESCE(created_by,''),COALESCE((SELECT display_name FROM users WHERE users.id=maps.created_by),''),created_at,updated_at`

func scanMap(row scanner) (domain.Map, error) {
	var item domain.Map
	var posterPresent int
	err := row.Scan(&item.ID, &item.NodeID, &item.Name, &item.Author, &item.Version, &item.Description, &posterPresent,
		&item.Health, &item.AggregateSHA256, &item.TotalSize,
		&item.SourceType, &item.SourceName, &item.RemovedFiles, &item.RemovedBytes, &item.CreatedBy, &item.CreatedByName,
		&item.CreatedAt, &item.UpdatedAt)
	if err == nil && posterPresent != 0 {
		item.PosterURL = "/api/v1/maps/" + item.ID + "/poster"
	}
	return item, err
}

func (s *Store) ListMaps(ctx context.Context, nodeID string, limit int) ([]domain.Map, error) {
	items, _, err := s.ListMapPage(ctx, nodeID, nil, nil, limit)
	return items, err
}

func (s *Store) GetMap(ctx context.Context, id string) (domain.Map, error) {
	item, err := scanMap(s.db.QueryRowContext(ctx, `SELECT `+mapColumns+` FROM maps WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Map{}, ErrNotFound
	}
	if err != nil {
		return domain.Map{}, err
	}
	if err := s.hydrateMap(ctx, &item); err != nil {
		return domain.Map{}, err
	}
	return item, nil
}

func (s *Store) hydrateMap(ctx context.Context, item *domain.Map) error {
	items := []domain.Map{*item}
	if err := s.hydrateMaps(ctx, items); err != nil {
		return err
	}
	*item = items[0]
	return nil
}

func (s *Store) GetMapPoster(ctx context.Context, id string) (string, string, []byte, error) {
	var nodeID, mime string
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT node_id,poster_mime,poster_data FROM maps WHERE id=?1`, id).Scan(&nodeID, &mime, &data)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (mime == "" || len(data) == 0)) {
		return "", "", nil, ErrNotFound
	}
	if err != nil {
		return "", "", nil, err
	}
	return nodeID, mime, data, nil
}

func (s *Store) QueueMapInstanceJob(ctx context.Context, mapID, instanceID, action, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if action != "map.assign" && action != "map.unassign" {
		return domain.Job{}, false, fmt.Errorf("unsupported map instance action %q", action)
	}
	if uuid.Validate(mapID) != nil || uuid.Validate(instanceID) != nil {
		return domain.Job{}, false, ErrNotFound
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
	if existing, reused, err := lookupIdempotentMapJob(ctx, tx, requestedBy, idempotencyKey, requestFingerprint); err != nil || reused {
		return existing, reused, err
	}

	var nodeID string
	var health domain.MapHealth
	if err := tx.QueryRowContext(ctx, `SELECT node_id,health FROM maps WHERE id=?1`, mapID).Scan(&nodeID, &health); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if health == domain.MapDeleting || action == "map.assign" && health == domain.MapBroken {
		return domain.Job{}, false, fmt.Errorf("map is not assignable: %w", ErrConflict)
	}
	var instanceNodeID, installationID string
	var status domain.InstanceStatus
	if err := tx.QueryRowContext(ctx, `SELECT node_id,status,COALESCE(installation_id,'') FROM instances WHERE id=?1`, instanceID).
		Scan(&instanceNodeID, &status, &installationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if nodeID != instanceNodeID {
		return domain.Job{}, false, fmt.Errorf("map and instance belong to different nodes: %w", ErrConflict)
	}
	if action == "map.unassign" && status != domain.InstanceStopped {
		return domain.Job{}, false, fmt.Errorf("instance must be stopped before removing a map: %w", ErrConflict)
	}
	payload := map[string]any{"map_id": mapID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instanceID, InstallationID: installationID,
		Type: action, Status: domain.JobQueued, Payload: payload, IdempotencyKey: idempotencyKey,
		RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,game_installation_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,NULLIF(?4,''),?5,?6,?7,NULLIF(?8,''),?9,?10,NULLIF(?11,''))`, job.ID, job.NodeID,
		job.InstanceID, job.InstallationID, job.Type, job.Status, string(raw), job.IdempotencyKey, nullableUUID(job.RequestedBy),
		job.CreatedAt, requestFingerprint)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}

func (s *Store) QueueMapNodeJob(ctx context.Context, mapID, action, scopeInstanceID, requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if action != "map.verify" && action != "map.inventory" {
		return domain.Job{}, false, fmt.Errorf("unsupported map node action %q", action)
	}
	if action != "map.verify" && scopeInstanceID != "" {
		return domain.Job{}, false, errors.New("map job instance scope is not supported for this action")
	}
	if uuid.Validate(mapID) != nil {
		return domain.Job{}, false, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback()
	if existing, reused, err := lookupIdempotentMapJob(ctx, tx, requestedBy, idempotencyKey, requestFingerprint); err != nil || reused {
		return existing, reused, err
	}
	var nodeID string
	var health domain.MapHealth
	if err := tx.QueryRowContext(ctx, `SELECT node_id,health FROM maps WHERE id=?1`, mapID).Scan(&nodeID, &health); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if health == domain.MapDeleting {
		return domain.Job{}, false, fmt.Errorf("map is being deleted: %w", ErrConflict)
	}
	payload := map[string]any{"map_id": mapID}
	if err := validateJobScopeInstanceTx(ctx, tx, nodeID, scopeInstanceID); err != nil {
		return domain.Job{}, false, err
	}
	if scopeInstanceID != "" {
		payload[jobScopeInstancePayloadField] = scopeInstanceID
	}
	raw, _ := json.Marshal(payload)
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, ScopeInstanceID: scopeInstanceID, Type: action, Status: domain.JobQueued, Payload: payload,
		IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,?4,?5,NULLIF(?6,''),?7,?8,NULLIF(?9,''))`, job.ID, job.NodeID, job.Type, job.Status,
		string(raw), job.IdempotencyKey, nullableUUID(job.RequestedBy), job.CreatedAt, requestFingerprint)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}

// QueueMapInventoryJobs reconciles Control's durable map records against an
// Agent after capability negotiation. At most one active inventory job exists
// for a map, including when an old connection and its replacement overlap.
func (s *Store) QueueMapInventoryJobs(ctx context.Context, nodeID string) ([]domain.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM maps WHERE node_id=?1 AND health<>'deleting' ORDER BY id`, nodeID)
	if err != nil {
		return nil, err
	}
	mapIDs := make([]string, 0)
	for rows.Next() {
		var mapID string
		if err := rows.Scan(&mapID); err != nil {
			rows.Close()
			return nil, err
		}
		mapIDs = append(mapIDs, mapID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	jobs := make([]domain.Job, 0, len(mapIDs))
	for _, mapID := range mapIDs {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE node_id=?1 AND type='map.inventory'
			AND status IN ('queued','dispatched','running') AND json_extract(payload,'$.map_id')=?2`, nodeID, mapID).Scan(&active); err != nil {
			return nil, err
		}
		if active != 0 {
			continue
		}
		payload := map[string]any{"map_id": mapID}
		raw, _ := json.Marshal(payload)
		job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, Type: "map.inventory", Status: domain.JobQueued,
			Payload: payload, CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,payload,created_at)
			VALUES(?1,?2,?3,?4,?5,?6)`, job.ID, job.NodeID, job.Type, job.Status, string(raw), job.CreatedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func lookupIdempotentMapJob(ctx context.Context, tx *sql.Tx, requestedBy, key, fingerprint string) (domain.Job, bool, error) {
	if key == "" {
		return domain.Job{}, false, nil
	}
	var row scanner
	if requestedBy == "" {
		row = tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by IS NULL AND idempotency_key=?1`, key)
	} else {
		row = tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, key)
	}
	job, stored, err := scanJobWithFingerprint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if !requestFingerprintsEqual(stored, fingerprint) {
		return domain.Job{}, false, ErrConflict
	}
	return job, true, tx.Commit()
}

func (s *Store) CreateMapDeleteConfirmation(ctx context.Context, mapID, requestedBy string) (string, time.Time, error) {
	if uuid.Validate(mapID) != nil || uuid.Validate(requestedBy) != nil {
		return "", time.Time{}, ErrNotFound
	}
	var assignments int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM map_assignments WHERE map_id=?1`, mapID).Scan(&assignments); err != nil {
		return "", time.Time{}, err
	}
	if assignments != 0 {
		return "", time.Time{}, fmt.Errorf("map is assigned to one or more instances: %w", ErrConflict)
	}
	var health domain.MapHealth
	if err := s.db.QueryRowContext(ctx, `SELECT health FROM maps WHERE id=?1`, mapID).Scan(&health); errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	} else if err != nil {
		return "", time.Time{}, err
	}
	if health == domain.MapDeleting {
		return "", time.Time{}, fmt.Errorf("map is already being deleted: %w", ErrConflict)
	}
	random, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	raw := "mdc_" + random
	now := time.Now().UTC()
	expiresAt := now.Add(mapDeleteConfirmationTTL)
	_, err = s.db.ExecContext(ctx, `INSERT INTO map_delete_confirmations(id,map_id,requested_by,token_hash,expires_at,created_at)
		VALUES(?1,?2,?3,?4,?5,?6)`, uuid.NewString(), mapID, requestedBy, tokenHash(raw), expiresAt, now)
	return raw, expiresAt, err
}

func (s *Store) QueueMapDelete(ctx context.Context, mapID, scopeInstanceID, requestedBy, confirmationToken, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if !strings.HasPrefix(confirmationToken, "mdc_") || len(confirmationToken) > 128 {
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
	var confirmationID, confirmedMapID, confirmedBy, jobID string
	var expiresAt time.Time
	var consumedAt *time.Time
	err = tx.QueryRowContext(ctx, `SELECT id,map_id,requested_by,expires_at,consumed_at,COALESCE(job_id,'')
		FROM map_delete_confirmations WHERE token_hash=?1`, tokenHash(confirmationToken)).Scan(
		&confirmationID, &confirmedMapID, &confirmedBy, &expiresAt, &consumedAt, &jobID)
	if errors.Is(err, sql.ErrNoRows) || confirmedMapID != mapID || confirmedBy != requestedBy || !expiresAt.After(time.Now().UTC()) {
		return domain.Job{}, false, ErrUnauthorized
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if consumedAt != nil {
		if jobID != "" {
			existing, stored, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE id=?1`, jobID))
			if lookupErr == nil && existing.IdempotencyKey == idempotencyKey && requestFingerprintsEqual(stored, requestFingerprint) {
				return existing, true, tx.Commit()
			}
		}
		return domain.Job{}, false, fmt.Errorf("map deletion confirmation has already been consumed: %w", ErrConflict)
	}
	if existing, reused, err := lookupIdempotentMapJob(ctx, tx, requestedBy, idempotencyKey, requestFingerprint); err != nil || reused {
		return existing, reused, err
	}
	var nodeID string
	var assignments, activeJobs int
	if err := tx.QueryRowContext(ctx, `SELECT node_id,(SELECT count(*) FROM map_assignments WHERE map_id=maps.id),
		(SELECT count(*) FROM jobs WHERE status IN ('queued','dispatched','running') AND json_extract(payload,'$.map_id')=maps.id)
		FROM maps WHERE id=?1`, mapID).Scan(&nodeID, &assignments, &activeJobs); errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	} else if err != nil {
		return domain.Job{}, false, err
	}
	if assignments != 0 || activeJobs != 0 {
		return domain.Job{}, false, fmt.Errorf("map has assignments or active operations: %w", ErrConflict)
	}
	payload := map[string]any{"map_id": mapID}
	if err := validateJobScopeInstanceTx(ctx, tx, nodeID, scopeInstanceID); err != nil {
		return domain.Job{}, false, err
	}
	if scopeInstanceID != "" {
		payload[jobScopeInstancePayloadField] = scopeInstanceID
	}
	raw, _ := json.Marshal(payload)
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, ScopeInstanceID: scopeInstanceID, Type: "map.delete", Status: domain.JobQueued, Payload: payload,
		IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,?4,?5,NULLIF(?6,''),?7,?8,NULLIF(?9,''))`, job.ID, job.NodeID, job.Type, job.Status,
		string(raw), job.IdempotencyKey, nullableUUID(job.RequestedBy), job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE maps SET health='deleting',updated_at=?2 WHERE id=?1`, mapID, job.CreatedAt); err != nil {
		return domain.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE map_delete_confirmations SET consumed_at=?2,job_id=?3 WHERE id=?1`, confirmationID, job.CreatedAt, job.ID); err != nil {
		return domain.Job{}, false, err
	}
	return job, false, tx.Commit()
}
