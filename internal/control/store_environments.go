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

const environmentColumns = `environment.id,environment.node_id,node.name,node.status,environment.system_dependencies,
	environment.steamcmd_source,environment.steamcmd_path,environment.steamcmd_status,environment.steamcmd_version,
	environment.last_error,environment.last_inspected_at,environment.created_at,environment.updated_at`

func scanEnvironment(row scanner) (domain.RuntimeEnvironment, error) {
	var item domain.RuntimeEnvironment
	var dependencies []byte
	err := row.Scan(&item.ID, &item.NodeID, &item.NodeName, &item.NodeStatus, &dependencies, &item.SteamCMDSource,
		&item.SteamCMDPath, &item.SteamCMDStatus, &item.SteamCMDVersion, &item.LastError, &item.LastInspectedAt,
		&item.CreatedAt, &item.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(dependencies, &item.SystemDependencies)
	}
	if item.SystemDependencies == nil {
		item.SystemDependencies = map[string]any{}
	}
	return item, err
}

func (s *Store) ListEnvironments(ctx context.Context) ([]domain.RuntimeEnvironment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+environmentColumns+` FROM environments environment
		JOIN nodes node ON node.id=environment.node_id ORDER BY node.name,node.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.RuntimeEnvironment, 0)
	for rows.Next() {
		item, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetEnvironment(ctx context.Context, id string) (domain.RuntimeEnvironment, error) {
	item, err := scanEnvironment(s.db.QueryRowContext(ctx, `SELECT `+environmentColumns+` FROM environments environment
		JOIN nodes node ON node.id=environment.node_id WHERE environment.id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RuntimeEnvironment{}, ErrNotFound
	}
	return item, err
}

const installationColumns = `installation.id,installation.environment_id,installation.node_id,installation.path,
	installation.app_id,installation.status,installation.build_id,installation.last_error,
	COALESCE((SELECT instance.id FROM instances instance WHERE instance.installation_id=installation.id),''),
	installation.last_checked_at,installation.created_at,installation.updated_at`

func scanGameInstallation(row scanner) (domain.GameInstallation, error) {
	var item domain.GameInstallation
	err := row.Scan(&item.ID, &item.EnvironmentID, &item.NodeID, &item.Path, &item.AppID,
		&item.Status, &item.BuildID, &item.LastError, &item.BoundInstanceID, &item.LastCheckedAt, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}

func (s *Store) ListGameInstallations(ctx context.Context, environmentID string) ([]domain.GameInstallation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+installationColumns+` FROM game_installations installation
		WHERE installation.environment_id=?1 ORDER BY installation.created_at,installation.id`, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.GameInstallation, 0)
	for rows.Next() {
		item, err := scanGameInstallation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetGameInstallation(ctx context.Context, id string) (domain.GameInstallation, error) {
	item, err := scanGameInstallation(s.db.QueryRowContext(ctx, `SELECT `+installationColumns+`
		FROM game_installations installation WHERE installation.id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.GameInstallation{}, ErrNotFound
	}
	return item, err
}

func (s *Store) CreateGameInstallation(ctx context.Context, environmentID, installationPath string) (domain.GameInstallation, error) {
	var nodeID, nodeOS string
	if err := s.db.QueryRowContext(ctx, `SELECT environment.node_id,node.os FROM environments environment
		JOIN nodes node ON node.id=environment.node_id WHERE environment.id=?1`, environmentID).Scan(&nodeID, &nodeOS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.GameInstallation{}, ErrNotFound
		}
		return domain.GameInstallation{}, err
	}
	var err error
	installationPath, err = normalizeGameInstallationPath(nodeOS, installationPath)
	if err != nil {
		return domain.GameInstallation{}, err
	}
	now := time.Now().UTC()
	pathKey := gameInstallationPathKey(nodeOS, installationPath)
	item := domain.GameInstallation{ID: uuid.NewString(), EnvironmentID: environmentID, NodeID: nodeID,
		Path: installationPath, AppID: 222860, Status: "pending", CreatedAt: now, UpdatedAt: now}
	_, err = s.db.ExecContext(ctx, `INSERT INTO game_installations(id,environment_id,node_id,path,path_key,app_id,status,created_at,updated_at)
		VALUES(?1,?2,?3,?4,?5,222860,'pending',?6,?6)`, item.ID, item.EnvironmentID, item.NodeID, item.Path, pathKey, now)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.GameInstallation{}, ErrConflict
		}
		return domain.GameInstallation{}, err
	}
	return item, nil
}

func normalizeGameInstallationPath(nodeOS, installationPath string) (string, error) {
	installationPath = strings.TrimSpace(strings.ReplaceAll(installationPath, "\\", "/"))
	if len(installationPath) < 1 || len(installationPath) > 1024 || strings.Contains(installationPath, "\x00") {
		return "", errors.New("invalid game installation path")
	}
	windows := strings.Contains(strings.ToLower(nodeOS), "windows")
	absolute := strings.HasPrefix(installationPath, "/")
	start := 0
	if windows {
		absolute = len(installationPath) >= 3 && installationPath[1] == ':' && installationPath[2] == '/' &&
			((installationPath[0] >= 'A' && installationPath[0] <= 'Z') || (installationPath[0] >= 'a' && installationPath[0] <= 'z'))
		if absolute {
			installationPath = strings.ToUpper(installationPath[:1]) + installationPath[1:]
			start = 1
		}
	} else if strings.Contains(installationPath, ":") {
		// Colons are valid on Linux, but rejecting them keeps the stored path
		// portable and avoids accidentally accepting a Windows path for a Linux node.
		return "", errors.New("invalid Linux game installation path")
	}
	if !absolute || strings.HasPrefix(installationPath, "//") {
		return "", errors.New("game installation path must be absolute")
	}
	components := strings.Split(installationPath, "/")
	for index, component := range components {
		if index < start || (absolute && index == start && component == "") {
			continue
		}
		if component == "" || component == "." || component == ".." {
			return "", errors.New("invalid game installation path")
		}
	}
	return installationPath, nil
}

func gameInstallationPathKey(nodeOS, installationPath string) string {
	if strings.Contains(strings.ToLower(nodeOS), "windows") {
		return strings.ToLower(installationPath)
	}
	return installationPath
}

func (s *Store) UpdateGameInstallationLocation(ctx context.Context, id, installationPath string) (domain.GameInstallation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.GameInstallation{}, err
	}
	defer tx.Rollback()
	var nodeOS, boundID, boundStatus string
	var boundPID int
	err = tx.QueryRowContext(ctx, `SELECT node.os,
		COALESCE(instance.id,''),COALESCE(instance.status,''),COALESCE(instance.pid,0)
		FROM game_installations installation JOIN nodes node ON node.id=installation.node_id
		LEFT JOIN instances instance ON instance.installation_id=installation.id WHERE installation.id=?1`, id).
		Scan(&nodeOS, &boundID, &boundStatus, &boundPID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.GameInstallation{}, ErrNotFound
	}
	if err != nil {
		return domain.GameInstallation{}, err
	}
	installationPath, err = normalizeGameInstallationPath(nodeOS, installationPath)
	if err != nil {
		return domain.GameInstallation{}, err
	}
	if boundID != "" && (boundPID != 0 || (boundStatus != string(domain.InstanceStopped) && boundStatus != string(domain.InstanceDegraded))) {
		return domain.GameInstallation{}, fmt.Errorf("bound instance is not safely stopped: %w", ErrConflict)
	}
	if boundID != "" {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE instance_id=?1 AND holds_instance_write_lock=1)`, boundID).Scan(&active); err != nil {
			return domain.GameInstallation{}, err
		}
		if active {
			return domain.GameInstallation{}, fmt.Errorf("bound instance has an active write job: %w", ErrConflict)
		}
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `UPDATE game_installations SET path=?2,path_key=?3,
		status='pending',build_id='',last_error='',last_checked_at=NULL,updated_at=?4 WHERE id=?1`,
		id, installationPath, gameInstallationPathKey(nodeOS, installationPath), now)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.GameInstallation{}, fmt.Errorf("game installation path conflicts with an existing record: %w", ErrConflict)
		}
		return domain.GameInstallation{}, err
	}
	if boundID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET path=?2,status='degraded',updated_at=?3 WHERE id=?1`, boundID, installationPath, now); err != nil {
			return domain.GameInstallation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.GameInstallation{}, err
	}
	return s.GetGameInstallation(ctx, id)
}

func (s *Store) MarkGameInstallationQueueFailed(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE game_installations
		SET status='degraded',last_error='operation could not be queued',updated_at=?2 WHERE id=?1 AND status='pending'`, id, time.Now().UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RemovePendingGameInstallation(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM game_installations WHERE id=?1 AND status='pending'
		AND NOT EXISTS(SELECT 1 FROM instances WHERE installation_id=game_installations.id)`, id)
	return err
}

var environmentJobTypes = map[string]bool{
	"environment.inspect": true, "steamcmd.configure": true, "steamcmd.clear": true, "steamcmd.install": true,
	"steamcmd.repair": true, "game.install": true, "game.import": true, "game.validate": true,
	"game.update": true, "game.delete": true,
}

func (s *Store) QueueEnvironmentJob(ctx context.Context, environmentID, installationID, action string, payload map[string]any,
	requestedBy, idempotencyKey, requestFingerprint string) (domain.Job, bool, error) {
	if !environmentJobTypes[action] {
		return domain.Job{}, false, fmt.Errorf("unsupported environment action %q", action)
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
		var fingerprint string
		var lookupErr error
		if requestedBy == "" {
			existing, fingerprint, lookupErr = scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by IS NULL AND idempotency_key=?1`, idempotencyKey))
		} else {
			existing, fingerprint, lookupErr = scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, requestedBy, idempotencyKey))
		}
		if lookupErr == nil {
			if !requestFingerprintsEqual(fingerprint, requestFingerprint) {
				return domain.Job{}, false, ErrConflict
			}
			return existing, true, tx.Commit()
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.Job{}, false, lookupErr
		}
	}
	var nodeID string
	var nodeStatus domain.NodeStatus
	if err := tx.QueryRowContext(ctx, `SELECT environment.node_id,node.status FROM environments environment
		JOIN nodes node ON node.id=environment.node_id WHERE environment.id=?1`, environmentID).Scan(&nodeID, &nodeStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, ErrNotFound
		}
		return domain.Job{}, false, err
	}
	if nodeStatus == domain.NodeRevoked {
		return domain.Job{}, false, ErrUnauthorized
	}
	boundInstanceID := ""
	if strings.HasPrefix(action, "game.") {
		var installationNodeID, boundStatus string
		var boundPID int
		if err := tx.QueryRowContext(ctx, `SELECT installation.node_id,COALESCE(instance.id,''),
			COALESCE(instance.status,''),COALESCE(instance.pid,0) FROM game_installations installation
			LEFT JOIN instances instance ON instance.installation_id=installation.id
			WHERE installation.id=?1 AND installation.environment_id=?2`, installationID, environmentID).
			Scan(&installationNodeID, &boundInstanceID, &boundStatus, &boundPID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.Job{}, false, ErrNotFound
			}
			return domain.Job{}, false, err
		}
		if installationNodeID != nodeID {
			return domain.Job{}, false, ErrConflict
		}
		if boundInstanceID != "" && action == "game.delete" {
			return domain.Job{}, false, fmt.Errorf("bound game installation cannot be deleted: %w", ErrConflict)
		}
		if boundInstanceID != "" && action == "game.install" {
			return domain.Job{}, false, fmt.Errorf("bound game installation cannot be installed again: %w", ErrConflict)
		}
		if boundInstanceID != "" && (boundPID != 0 || (boundStatus != string(domain.InstanceStopped) && boundStatus != string(domain.InstanceDegraded))) {
			return domain.Job{}, false, fmt.Errorf("bound instance must be stopped before game maintenance: %w", ErrConflict)
		}
		installationID = strings.TrimSpace(installationID)
	}
	steamConfiguration := strings.HasPrefix(action, "steamcmd.")
	steamConsumer := action == "game.install" || action == "game.validate" || action == "game.update"
	if steamConsumer {
		var steamStatus string
		if err := tx.QueryRowContext(ctx, `SELECT steamcmd_status FROM environments WHERE id=?1`, environmentID).Scan(&steamStatus); err != nil {
			return domain.Job{}, false, err
		}
		if steamStatus != "ready" {
			return domain.Job{}, false, fmt.Errorf("SteamCMD is not ready: %w", ErrConflict)
		}
	}
	if steamConfiguration || steamConsumer {
		var conflicting bool
		query := `SELECT EXISTS(SELECT 1 FROM jobs WHERE node_id=?1 AND status IN ('queued','dispatched','running') AND `
		if steamConfiguration {
			// A game.reinstall task may still be active after upgrading from a
			// previous Control version. It can no longer be queued, but must keep
			// blocking SteamCMD reconfiguration until its durable terminal state.
			query += `type IN ('steamcmd.configure','steamcmd.clear','steamcmd.install','steamcmd.repair','game.install','game.validate','game.update','game.reinstall'))`
		} else {
			query += `type IN ('steamcmd.configure','steamcmd.clear','steamcmd.install','steamcmd.repair'))`
		}
		if err := tx.QueryRowContext(ctx, query, nodeID).Scan(&conflicting); err != nil {
			return domain.Job{}, false, err
		}
		if conflicting {
			return domain.Job{}, false, ErrConflict
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	now := time.Now().UTC()
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: boundInstanceID, InstallationID: installationID, Type: action,
		Status: domain.JobQueued, Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: requestedBy, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,game_installation_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,NULLIF(?3,''),NULLIF(?4,''),?5,'queued',?6,NULLIF(?7,''),?8,?9,NULLIF(?10,''))`, job.ID, nodeID,
		job.InstanceID, installationID, action, string(raw), idempotencyKey, nullableUUID(requestedBy), now, requestFingerprint)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.Job{}, false, ErrConflict
		}
		return domain.Job{}, false, err
	}
	if installationID != "" {
		status := map[string]string{"game.install": "installing", "game.import": "validating", "game.validate": "validating",
			"game.update": "updating"}[action]
		if status != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE game_installations SET status=?2,last_error='',updated_at=?3 WHERE id=?1`, installationID, status, now); err != nil {
				return domain.Job{}, false, err
			}
		}
	}
	return job, false, tx.Commit()
}
