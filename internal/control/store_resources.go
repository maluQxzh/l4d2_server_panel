package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type scanner interface{ Scan(...any) error }

func scanNode(row scanner) (domain.Node, error) {
	var n domain.Node
	var memoryUsed, memoryTotal, diskUsed, diskTotal int64
	err := row.Scan(&n.ID, &n.Name, &n.Status, &n.OS, &n.Arch, &n.AgentVersion, &n.Address, &n.PublicAddress, &n.InstanceRoot,
		&n.CPUPercent, &memoryUsed, &memoryTotal, &diskUsed, &diskTotal, &n.LastSeenAt, &n.CreatedAt)
	if err == nil {
		n.MemoryUsed, n.MemoryTotal = uint64(memoryUsed), uint64(memoryTotal)
		n.DiskUsed, n.DiskTotal = uint64(diskUsed), uint64(diskTotal)
	}
	return n, err
}

func scanNodeResource(row scanner) (domain.Node, error) {
	var n domain.Node
	var memoryUsed, memoryTotal, diskUsed, diskTotal, networkRx, networkTx int64
	err := row.Scan(&n.ID, &n.Name, &n.Status, &n.OS, &n.Arch, &n.AgentVersion, &n.Address, &n.PublicAddress, &n.InstanceRoot,
		&n.CPUPercent, &memoryUsed, &memoryTotal, &diskUsed, &diskTotal, &n.LastSeenAt, &n.CreatedAt, &networkRx, &networkTx)
	if err == nil {
		n.MemoryUsed, n.MemoryTotal = uint64(memoryUsed), uint64(memoryTotal)
		n.DiskUsed, n.DiskTotal = uint64(diskUsed), uint64(diskTotal)
		n.NetworkRx, n.NetworkTx = uint64(networkRx), uint64(networkTx)
	}
	return n, err
}

const nodeColumns = `id,name,status,os,arch,agent_version,address,public_address,instance_root,cpu_percent,
		memory_used,memory_total,disk_used,disk_total,last_seen_at,created_at`

const nodeResourceColumns = nodeColumns + `,
	COALESCE((SELECT metric.network_rx FROM metrics metric WHERE metric.node_id=nodes.id AND metric.instance_id IS NULL ORDER BY metric.collected_at DESC LIMIT 1),0),
	COALESCE((SELECT metric.network_tx FROM metrics metric WHERE metric.node_id=nodes.id AND metric.instance_id IS NULL ORDER BY metric.collected_at DESC LIMIT 1),0)`

func (s *Store) CreateEnrollmentToken(ctx context.Context, label, createdBy string, ttl time.Duration) (string, time.Time, error) {
	if ttl < time.Minute || ttl > 7*24*time.Hour {
		return "", time.Time{}, errors.New("enrollment token TTL must be between one minute and seven days")
	}
	label = strings.TrimSpace(label)
	if len(label) > 128 {
		return "", time.Time{}, errors.New("node name must be at most 128 characters")
	}
	raw, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().UTC().Add(ttl)
	_, err = s.db.ExecContext(ctx, `INSERT INTO enrollment_tokens(id,token_hash,label,created_by,expires_at)
		VALUES(?1,?2,?3,?4,?5)`, uuid.NewString(), tokenHash(raw), label, nullableUUID(createdBy), expires)
	return raw, expires, err
}

type EnrollRequest struct {
	Token        string `json:"token"`
	Name         string `json:"name"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
	InstanceRoot string `json:"instance_root"`
}

func (s *Store) ConsumeEnrollmentToken(ctx context.Context, req EnrollRequest, remoteAddress string) (domain.Node, string, error) {
	if strings.TrimSpace(req.Token) == "" || strings.TrimSpace(req.InstanceRoot) == "" {
		return domain.Node{}, "", errors.New("token and instance_root are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Node{}, "", err
	}
	defer tx.Rollback()
	var tokenID, tokenLabel string
	err = tx.QueryRowContext(ctx, `SELECT id,label FROM enrollment_tokens WHERE token_hash=?1 AND consumed_at IS NULL AND expires_at>?2`, tokenHash(req.Token), time.Now().UTC()).Scan(&tokenID, &tokenLabel)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Node{}, "", ErrUnauthorized
	}
	if err != nil {
		return domain.Node{}, "", err
	}
	nodeName := strings.TrimSpace(tokenLabel)
	if nodeName == "" {
		nodeName = strings.TrimSpace(req.Name)
	}
	if nodeName == "" || len(nodeName) > 128 {
		return domain.Node{}, "", errors.New("node name must be between 1 and 128 characters")
	}
	credential, err := randomToken(48)
	if err != nil {
		return domain.Node{}, "", err
	}
	node := domain.Node{
		ID: uuid.NewString(), Name: nodeName, Status: domain.NodeOffline,
		OS: strings.ToLower(strings.TrimSpace(req.OS)), Arch: strings.ToLower(strings.TrimSpace(req.Arch)),
		AgentVersion: strings.TrimSpace(req.AgentVersion), Address: remoteAddress,
		InstanceRoot: strings.TrimSpace(req.InstanceRoot), CreatedAt: time.Now().UTC(),
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id,name,status,os,arch,agent_version,address,instance_root,agent_credential_hash,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)`, node.ID, node.Name, node.Status, node.OS, node.Arch,
		node.AgentVersion, node.Address, node.InstanceRoot, tokenHash(credential), node.CreatedAt)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE enrollment_tokens SET consumed_at=unixepoch('subsec')*1000000 WHERE id=?1`, tokenID)
	}
	if err != nil {
		return domain.Node{}, "", err
	}
	return node, credential, tx.Commit()
}

func (s *Store) NodeByCredential(ctx context.Context, credential string) (domain.Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE agent_credential_hash=?1 AND status<>'revoked'`, tokenHash(credential)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Node{}, ErrUnauthorized
	}
	return n, err
}

func (s *Store) ListNodes(ctx context.Context) ([]domain.Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeResourceColumns+` FROM nodes ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Node, 0)
	for rows.Next() {
		n, err := scanNodeResource(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return items, rows.Err()
}

func (s *Store) GetNode(ctx context.Context, id string) (domain.Node, error) {
	n, err := scanNodeResource(s.db.QueryRowContext(ctx, `SELECT `+nodeResourceColumns+` FROM nodes WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Node{}, ErrNotFound
	}
	return n, err
}

func (s *Store) RenameNode(ctx context.Context, id, name string) (domain.Node, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return domain.Node{}, errors.New("node name must be between 1 and 128 characters")
	}
	tag, err := s.db.ExecContext(ctx, `UPDATE nodes SET name=?2 WHERE id=?1`, id, name)
	if err != nil {
		return domain.Node{}, err
	}
	if resultRowsAffected(tag) == 0 {
		return domain.Node{}, ErrNotFound
	}
	return s.GetNode(ctx, id)
}

func (s *Store) UpdateNode(ctx context.Context, id, name string, publicAddress *string) (domain.Node, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return domain.Node{}, errors.New("node name must be between 1 and 128 characters")
	}
	var normalized any
	if publicAddress != nil {
		value := strings.TrimSpace(*publicAddress)
		if err := validatePublicAddress(value); err != nil {
			return domain.Node{}, err
		}
		normalized = value
	}
	tag, err := s.db.ExecContext(ctx, `UPDATE nodes SET name=?2,public_address=COALESCE(?3,public_address) WHERE id=?1`, id, name, normalized)
	if err != nil {
		return domain.Node{}, err
	}
	if resultRowsAffected(tag) == 0 {
		return domain.Node{}, ErrNotFound
	}
	return s.GetNode(ctx, id)
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=?1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	// Notifications are intentionally not foreign-keyed because their resource
	// can be a node, instance, job, or another domain object. Remove every
	// notification whose direct resource or routing metadata belongs to this
	// node before deleting the referenced rows. Audit and event history remain
	// immutable/retained according to their existing policies.
	if _, err := tx.ExecContext(ctx, `DELETE FROM notifications
		WHERE resource=?1
			OR resource IN (SELECT id FROM instances WHERE node_id=?1)
			OR resource IN (SELECT id FROM jobs WHERE node_id=?1)
			OR json_extract(metadata,'$.node_id')=?1
			OR json_extract(metadata,'$.instance_id') IN (SELECT id FROM instances WHERE node_id=?1)
			OR json_extract(metadata,'$.job_id') IN (SELECT id FROM jobs WHERE node_id=?1)`, id); err != nil {
		return err
	}
	// These tables deliberately RESTRICT deletion because their normal lifecycle
	// is task-driven. Administrative node deletion is the one bounded path that
	// removes the complete Control-plane projection in dependency order.
	for _, statement := range []string{
		`DELETE FROM quarantine_entries WHERE node_id=?1`,
		`DELETE FROM maps WHERE node_id=?1`,
		`DELETE FROM instances WHERE node_id=?1`,
		`DELETE FROM jobs WHERE node_id=?1`,
		`DELETE FROM nodes WHERE id=?1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RotateNodeCredential(ctx context.Context, id string) (string, error) {
	credential, err := randomToken(48)
	if err != nil {
		return "", err
	}
	tag, err := s.db.ExecContext(ctx, `UPDATE nodes SET agent_credential_hash=?2 WHERE id=?1 AND status<>'revoked'`, id, tokenHash(credential))
	if err != nil {
		return "", err
	}
	if resultRowsAffected(tag) == 0 {
		return "", ErrNotFound
	}
	return credential, nil
}

func (s *Store) SetNodeConnected(ctx context.Context, id, address, version string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET status='online',address=COALESCE(NULLIF(?2,''),address),
		agent_version=COALESCE(NULLIF(?3,''),agent_version),last_seen_at=unixepoch('subsec')*1000000 WHERE id=?1`, id, address, version)
	return err
}

func (s *Store) SetNodeOffline(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET status=CASE WHEN status='revoked' THEN status ELSE 'offline' END WHERE id=?1`, id)
	return err
}

func (s *Store) Housekeeping(ctx context.Context) error {
	now := time.Now().UTC()
	hourAgo := now.Add(-time.Hour)
	dayAgo := now.Add(-24 * time.Hour)
	monthAgo := now.Add(-30 * 24 * time.Hour)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<?1`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE requested_by IS NULL AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update')
		AND status IN ('succeeded','canceled') AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1
		AND finished_at<?1`, hourAgo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE requested_by IS NULL AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update')
		AND status='failed' AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1
		AND finished_at<?1`, dayAgo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE type IN ('logs.search','logs.chunk','backup.chunk','backup.upload.chunk')
		AND status IN ('succeeded','failed','canceled') AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1
		AND finished_at<?1`, dayAgo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE backup_uploads SET status='expired',updated_at=?1
		WHERE status IN ('uploading','ready') AND expires_at<=?1
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.type='backup.upload.chunk'
			AND json_extract(j.payload,'$.upload_id')=backup_uploads.id AND (
				j.status IN ('queued','dispatched','running') OR
				(j.status='succeeded' AND j.id IS NOT backup_uploads.last_chunk_job_id
					AND json_extract(j.payload,'$.offset')=backup_uploads.received_size)))`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM backup_uploads WHERE status IN ('expired','failed','succeeded')
		AND updated_at<?1`, monthAgo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET
		payload=json_remove(payload,'$.command_ciphertext','$.source_ciphertext','$.content_ciphertext','$.content_base64_ciphertext','$.archive_ciphertext','$.keyword_ciphertext','$.existing_rcon_password_ciphertext'),
		result=json_remove(result,'$.response_ciphertext','$.content_base64_ciphertext','$.entries_ciphertext')
		WHERE status IN ('succeeded','failed','canceled') AND finished_at<?1
		AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1
		AND (json_type(payload,'$.command_ciphertext') IS NOT NULL OR json_type(payload,'$.source_ciphertext') IS NOT NULL
			OR json_type(payload,'$.content_ciphertext') IS NOT NULL OR json_type(payload,'$.content_base64_ciphertext') IS NOT NULL
			OR json_type(payload,'$.archive_ciphertext') IS NOT NULL OR json_type(payload,'$.keyword_ciphertext') IS NOT NULL
			OR json_type(payload,'$.existing_rcon_password_ciphertext') IS NOT NULL OR json_type(result,'$.response_ciphertext') IS NOT NULL
			OR json_type(result,'$.content_base64_ciphertext') IS NOT NULL OR json_type(result,'$.entries_ciphertext') IS NOT NULL)`, dayAgo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_log WHERE created_at<?1`, monthAgo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM player_bans WHERE expires_at IS NOT NULL AND expires_at<=?1`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quarantine_purge_confirmations
		WHERE expires_at<?1 OR consumed_at<?1`, dayAgo); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.MaintainSQLite(ctx)
}

func (s *Store) MarkStaleNodes(ctx context.Context) ([]domain.Node, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE nodes SET status='offline' WHERE status='online' AND last_seen_at<?1
		RETURNING `+nodeColumns, time.Now().UTC().Add(-35*time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Node, 0)
	for rows.Next() {
		item, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Heartbeat(ctx context.Context, nodeID, agentVersion string, statuses map[string]string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET status='online',agent_version=?2,last_seen_at=?3 WHERE id=?1 AND status<>'revoked'`, nodeID, agentVersion, now); err != nil {
		return err
	}
	allowed := map[string]bool{"installing": true, "stopped": true, "starting": true, "running": true, "stopping": true, "updating": true, "degraded": true, "external": true}
	validStatuses := make(map[string]string, len(statuses))
	for instanceID, status := range statuses {
		if allowed[status] {
			validStatuses[instanceID] = status
		}
	}
	if len(validStatuses) == 0 {
		return tx.Commit()
	}
	instanceIDs := make([]string, 0, len(validStatuses))
	for instanceID := range validStatuses {
		instanceIDs = append(instanceIDs, instanceID)
	}
	sort.Strings(instanceIDs)
	for _, instanceID := range instanceIDs {
		result, err := tx.ExecContext(ctx, `UPDATE instances AS instance SET status=?3,updated_at=?4
			WHERE instance.id=?2 AND instance.node_id=?1 AND NOT EXISTS (
				SELECT 1 FROM jobs AS job WHERE job.instance_id=instance.id
				AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`)`, nodeID, instanceID, validStatuses[instanceID], now)
		if err != nil {
			return err
		}
		_ = result
	}
	return tx.Commit()
}

func (s *Store) SaveMetric(ctx context.Context, sample domain.MetricSample) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO metrics(node_id,instance_id,cpu_percent,memory_used,memory_total,disk_used,disk_total,
		network_rx,network_tx,players,collected_at) VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11)`,
		sample.NodeID, nullableUUID(sample.InstanceID), sample.CPUPercent, int64(sample.MemoryUsed), int64(sample.MemoryTotal),
		int64(sample.DiskUsed), int64(sample.DiskTotal), int64(sample.NetworkRx), int64(sample.NetworkTx), sample.Players, sample.CollectedAt)
	if err == nil && sample.InstanceID == "" {
		_, err = s.db.ExecContext(ctx, `UPDATE nodes SET cpu_percent=?2,memory_used=?3,memory_total=?4,disk_used=?5,disk_total=?6,last_seen_at=?7
			WHERE id=?1`, sample.NodeID, sample.CPUPercent, int64(sample.MemoryUsed), int64(sample.MemoryTotal), int64(sample.DiskUsed), int64(sample.DiskTotal), time.Now().UTC())
	}
	return err
}

func nullableUUID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

const instanceColumns = `id,node_id,COALESCE(installation_id,''),name,status,path,host,port,tv_port,pid,map,mode,players,max_players,build_id,
		version,config,started_at,created_at,updated_at,
		COALESCE((SELECT name FROM nodes WHERE nodes.id=instances.node_id),''),
		COALESCE((SELECT status FROM nodes WHERE nodes.id=instances.node_id),'offline'),
		COALESCE((SELECT public_address FROM nodes WHERE nodes.id=instances.node_id),''),
		COALESCE((SELECT state.available FROM instance_update_state state WHERE state.instance_id=instances.id),false),
		COALESCE((SELECT state.available_build FROM instance_update_state state WHERE state.instance_id=instances.id),''),
		(SELECT state.checked_at FROM instance_update_state state WHERE state.instance_id=instances.id)`

func scanInstance(row scanner) (domain.Instance, error) {
	var inst domain.Instance
	var raw []byte
	err := row.Scan(&inst.ID, &inst.NodeID, &inst.InstallationID, &inst.Name, &inst.Status, &inst.Path, &inst.Host, &inst.Port, &inst.TVPort,
		&inst.PID, &inst.Map, &inst.Mode, &inst.Players, &inst.MaxPlayers, &inst.BuildID, &inst.Version, &raw,
		&inst.StartedAt, &inst.CreatedAt, &inst.UpdatedAt, &inst.NodeName, &inst.NodeStatus, &inst.NodePublicAddress, &inst.UpdateAvailable,
		&inst.AvailableBuild, &inst.UpdateCheckedAt)
	if err == nil {
		err = json.Unmarshal(raw, &inst.Config)
	}
	return inst, err
}

func (s *Store) ListInstances(ctx context.Context, nodeID string) ([]domain.Instance, error) {
	query := `SELECT ` + instanceColumns + ` FROM instances`
	args := []any{}
	if nodeID != "" {
		query += ` WHERE node_id=?1`
		args = append(args, nodeID)
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Instance, 0)
	for rows.Next() {
		item, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetInstance(ctx context.Context, id string) (domain.Instance, error) {
	inst, err := scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, ErrNotFound
	}
	return inst, err
}

type CreateInstanceInput struct {
	NodeID                    string
	InstallationID            string
	ProvisionalInstallationID string
	Name                      string
	Path                      string
	Host                      string
	Port                      int
	TVPort                    int
	Config                    domain.InstanceConfig
}

func (s *Store) CreateInstance(ctx context.Context, input CreateInstanceInput) (domain.Instance, error) {
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 128 || !validInstancePorts(input.Port, input.TVPort) || !validInstanceAgentHost(input.Host) {
		return domain.Instance{}, errors.New("invalid instance name or port")
	}
	if input.Host == "" {
		input.Host = "0.0.0.0"
	}
	if input.Config.MaxPlayers == 0 {
		input.Config.MaxPlayers = 8
	}
	if strings.TrimSpace(input.Config.Hostname) == "" {
		input.Config.Hostname = strings.TrimSpace(input.Name)
	}
	config, err := json.Marshal(input.Config)
	if err != nil {
		return domain.Instance{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Instance{}, err
	}
	defer tx.Rollback()
	var nodeName string
	var nodeStatus domain.NodeStatus
	var nodePublicAddress string
	if err := tx.QueryRowContext(ctx, `SELECT name,status,COALESCE(public_address,'') FROM nodes WHERE id=?1`, input.NodeID).
		Scan(&nodeName, &nodeStatus, &nodePublicAddress); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Instance{}, ErrNotFound
		}
		return domain.Instance{}, err
	}
	if input.InstallationID != "" {
		var installationNodeID, installationStatus string
		var bound bool
		if err := tx.QueryRowContext(ctx, `SELECT installation.node_id,installation.status,
			EXISTS(SELECT 1 FROM instances WHERE installation_id=installation.id)
			FROM game_installations installation WHERE installation.id=?1`, input.InstallationID).Scan(&installationNodeID, &installationStatus, &bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.Instance{}, ErrNotFound
			}
			return domain.Instance{}, err
		}
		provisionalImport := installationStatus == "pending" && input.ProvisionalInstallationID != "" &&
			input.ProvisionalInstallationID == input.InstallationID
		if installationNodeID != input.NodeID || (installationStatus != "ready" && !provisionalImport) || bound {
			return domain.Instance{}, ErrConflict
		}
	}
	reserved := make(map[int]struct{})
	rows, err := tx.QueryContext(ctx, `SELECT port FROM instance_port_reservations WHERE node_id=?1
		UNION SELECT port FROM pending_instance_port_changes WHERE node_id=?1 ORDER BY port`, input.NodeID)
	if err != nil {
		return domain.Instance{}, err
	}
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			_ = rows.Close()
			return domain.Instance{}, err
		}
		reserved[port] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return domain.Instance{}, err
	}
	if _, occupied := reserved[input.TVPort]; input.TVPort != 0 && occupied {
		return domain.Instance{}, ErrConflict
	}
	if input.Port == 0 {
		for candidate := 27015; candidate <= 27999; candidate++ {
			if candidate == input.TVPort {
				continue
			}
			if _, occupied := reserved[candidate]; !occupied {
				input.Port = candidate
				break
			}
		}
		if input.Port == 0 {
			return domain.Instance{}, fmt.Errorf("no available game port on node: %w", ErrConflict)
		}
	} else if _, occupied := reserved[input.Port]; occupied {
		return domain.Instance{}, ErrConflict
	}
	inst := domain.Instance{ID: uuid.NewString(), NodeID: input.NodeID, NodeName: nodeName, NodeStatus: nodeStatus, NodePublicAddress: nodePublicAddress,
		InstallationID: input.InstallationID, Name: strings.TrimSpace(input.Name), Status: domain.InstanceInstalling,
		Path: input.Path, Host: input.Host, Port: input.Port, TVPort: input.TVPort, MaxPlayers: input.Config.MaxPlayers,
		Version: 1, Config: input.Config, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO instances(id,node_id,installation_id,name,status,path,host,port,tv_port,max_players,version,config,created_at,updated_at)
		VALUES(?1,?2,NULLIF(?3,''),?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14)`, inst.ID, inst.NodeID, inst.InstallationID, inst.Name, inst.Status, inst.Path,
		inst.Host, inst.Port, inst.TVPort, inst.MaxPlayers, inst.Version, string(config), inst.CreatedAt, inst.UpdatedAt)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.Instance{}, ErrConflict
		}
		return domain.Instance{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO instance_port_reservations(node_id,instance_id,port,kind)
		VALUES(?1,?2,?3,'game')`, inst.NodeID, inst.ID, inst.Port); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Instance{}, ErrConflict
		}
		return domain.Instance{}, err
	}
	if inst.TVPort != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO instance_port_reservations(node_id,instance_id,port,kind)
			VALUES(?1,?2,?3,'source_tv')`, inst.NodeID, inst.ID, inst.TVPort); err != nil {
			if isSQLiteConstraint(err) {
				return domain.Instance{}, ErrConflict
			}
			return domain.Instance{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.Instance{}, err
	}
	return inst, nil
}

func validInstancePorts(port, tvPort int) bool {
	if port < 0 || port > 65535 || tvPort < 0 || tvPort > 65535 {
		return false
	}
	return port == 0 || tvPort == 0 || port != tvPort
}

func (s *Store) UpdateInstance(ctx context.Context, id, name string, config *domain.InstanceConfig, expectedVersion int, _ string) (domain.Instance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Instance{}, err
	}
	defer tx.Rollback()
	current, err := scanInstance(tx.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, ErrNotFound
	}
	if err != nil {
		return domain.Instance{}, err
	}
	if expectedVersion > 0 && current.Version != expectedVersion {
		return domain.Instance{}, ErrConflict
	}
	if strings.TrimSpace(name) != "" {
		current.Name = strings.TrimSpace(name)
	}
	if config != nil {
		current.Config = *config
		current.MaxPlayers = config.MaxPlayers
	}
	raw, _ := json.Marshal(current.Config)
	current.Version++
	current.UpdatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, `UPDATE instances SET name=?2,config=?3,max_players=?4,version=?5,updated_at=?6 WHERE id=?1`,
		id, current.Name, string(raw), current.MaxPlayers, current.Version, current.UpdatedAt)
	if err != nil {
		return domain.Instance{}, err
	}
	return current, tx.Commit()
}

func (s *Store) UpdateInstanceAndQueue(ctx context.Context, id, name string, config *domain.InstanceConfig, port *int, expectedVersion int,
	userID, idempotencyKey string, rconPasswordCiphertext, serverPasswordCiphertext *string) (domain.Instance, domain.Job, bool, error) {
	request := map[string]any{"name": name, "config": config, "port": port, "version": expectedVersion}
	if serverPasswordCiphertext != nil {
		// This compatibility path cannot recover the plaintext after encryption.
		// HTTP handlers use UpdateInstanceAndQueueWithFingerprint and hash the
		// clear request before encryption; direct callers still fail safely on a
		// changed ciphertext.
		request["server_password_ciphertext"] = *serverPasswordCiphertext
	}
	if rconPasswordCiphertext != nil {
		request["rcon_password_ciphertext"] = *rconPasswordCiphertext
	}
	requestFingerprint, err := canonicalRequestFingerprint("instance-config", id, "configure", request)
	if err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	return s.UpdateInstanceAndQueueWithFingerprint(ctx, id, name, config, port, expectedVersion, userID, idempotencyKey,
		rconPasswordCiphertext, serverPasswordCiphertext, requestFingerprint)
}

func (s *Store) UpdateInstanceAndQueueWithFingerprint(ctx context.Context, id, name string, config *domain.InstanceConfig, port *int, expectedVersion int,
	userID, idempotencyKey string, rconPasswordCiphertext, serverPasswordCiphertext *string, requestFingerprint string) (domain.Instance, domain.Job, bool, error) {
	if len(idempotencyKey) > 200 {
		return domain.Instance{}, domain.Job{}, false, errors.New("idempotency key is too long")
	}
	if err := requireRequestFingerprint(idempotencyKey, requestFingerprint); err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	defer tx.Rollback()
	if idempotencyKey != "" {
		existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=?1 AND idempotency_key=?2`, userID, idempotencyKey))
		if lookupErr == nil {
			if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
				return domain.Instance{}, domain.Job{}, false, ErrConflict
			}
			current, getErr := scanInstance(tx.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=?1`, id))
			if getErr != nil {
				return domain.Instance{}, domain.Job{}, false, getErr
			}
			return current, existing, true, tx.Commit()
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.Instance{}, domain.Job{}, false, lookupErr
		}
	}
	current, err := scanInstance(tx.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instance{}, domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	if current.Status == domain.InstanceExternal {
		return domain.Instance{}, domain.Job{}, false, fmt.Errorf("external instance is read-only until a controlled restart: %w", ErrConflict)
	}
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE instance_id=?1
		AND (status IN ('queued','dispatched','running')
			OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1)
			OR (status IN ('succeeded','failed') AND json_extract(result,'$._projection_pending')=1))
		AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort')
		AND NOT (type='file' AND json_extract(payload,'$.operation')='read'))`, id).Scan(&busy); err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	if busy {
		return domain.Instance{}, domain.Job{}, false, ErrConflict
	}
	if expectedVersion > 0 && current.Version != expectedVersion {
		return domain.Instance{}, domain.Job{}, false, ErrConflict
	}
	desiredPort := current.Port
	portChanged := false
	if port != nil {
		desiredPort = *port
		if desiredPort <= 0 || desiredPort > 65535 {
			return domain.Instance{}, domain.Job{}, false, errors.New("game port must be between 1 and 65535")
		}
		if desiredPort == current.TVPort {
			return domain.Instance{}, domain.Job{}, false, fmt.Errorf("game port conflicts with the SourceTV port: %w", ErrConflict)
		}
		portChanged = desiredPort != current.Port
		if portChanged && current.Status != domain.InstanceStopped {
			return domain.Instance{}, domain.Job{}, false, fmt.Errorf("game port can only be changed while the instance is stopped: %w", ErrConflict)
		}
	}
	if strings.TrimSpace(name) != "" {
		current.Name = strings.TrimSpace(name)
	}
	if config != nil {
		config.ServerPassword = ""
		current.Config = *config
		current.MaxPlayers = config.MaxPlayers
	}
	if serverPasswordCiphertext != nil {
		tag, err := tx.ExecContext(ctx, `UPDATE instance_secrets SET server_password_ciphertext=NULLIF(?2,''),updated_at=unixepoch('subsec')*1000000 WHERE instance_id=?1`, id, *serverPasswordCiphertext)
		if err != nil {
			return domain.Instance{}, domain.Job{}, false, err
		}
		if resultRowsAffected(tag) == 0 {
			return domain.Instance{}, domain.Job{}, false, ErrNotFound
		}
	}
	if rconPasswordCiphertext != nil {
		tag, err := tx.ExecContext(ctx, `UPDATE instance_secrets SET rcon_ciphertext=?2,updated_at=unixepoch('subsec')*1000000 WHERE instance_id=?1`, id, *rconPasswordCiphertext)
		if err != nil {
			return domain.Instance{}, domain.Job{}, false, err
		}
		if resultRowsAffected(tag) == 0 {
			return domain.Instance{}, domain.Job{}, false, ErrNotFound
		}
	}
	rawConfig, _ := json.Marshal(current.Config)
	current.Version++
	current.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE instances SET name=?2,config=?3,max_players=?4,version=?5,updated_at=?6 WHERE id=?1`,
		id, current.Name, string(rawConfig), current.MaxPlayers, current.Version, current.UpdatedAt); err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	desired := current
	desired.Port = desiredPort
	payload := map[string]any{"instance": desired}
	rawPayload, _ := json.Marshal(payload)
	job := domain.Job{ID: uuid.NewString(), NodeID: current.NodeID, InstanceID: current.ID, Type: "configure", Status: domain.JobQueued,
		Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: userID, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES(?1,?2,?3,?4,?5,?6,NULLIF(?7,''),?8,?9,NULLIF(?10,''))`, job.ID, job.NodeID, job.InstanceID, job.Type, job.Status,
		string(rawPayload), job.IdempotencyKey, job.RequestedBy, job.CreatedAt, requestFingerprint); err != nil {
		if isSQLiteConstraint(err) {
			return domain.Instance{}, domain.Job{}, false, ErrConflict
		}
		return domain.Instance{}, domain.Job{}, false, err
	}
	if portChanged {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pending_instance_port_changes(job_id,instance_id,node_id,port)
			VALUES(?1,?2,?3,?4)`, job.ID, current.ID, current.NodeID, desiredPort); err != nil {
			if isSQLiteConstraint(err) {
				return domain.Instance{}, domain.Job{}, false, ErrConflict
			}
			return domain.Instance{}, domain.Job{}, false, err
		}
	}
	return current, job, false, tx.Commit()
}

func (s *Store) RemoveInstanceRecord(ctx context.Context, id string) error {
	tag, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE id=?1 AND NOT EXISTS (
		SELECT 1 FROM jobs WHERE instance_id=?1
		AND (status IN ('queued','dispatched','running')
			OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1)
			OR (status IN ('succeeded','failed') AND json_extract(result,'$._projection_pending')=1))
		AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort')
		AND NOT (type='file' AND json_extract(payload,'$.operation')='read'))`, id)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrConflict
	}
	return err
}

func (s *Store) DeleteInstanceAfterJob(ctx context.Context, id string) error {
	tag, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE id=?1`, id)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SaveInstanceSecrets(ctx context.Context, instanceID, rconCiphertext, serverPasswordCiphertext string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO instance_secrets(instance_id,rcon_ciphertext,server_password_ciphertext) VALUES(?1,?2,NULLIF(?3,''))
		ON CONFLICT(instance_id) DO UPDATE SET rcon_ciphertext=EXCLUDED.rcon_ciphertext,
		server_password_ciphertext=EXCLUDED.server_password_ciphertext,updated_at=unixepoch('subsec')*1000000`, instanceID, rconCiphertext, serverPasswordCiphertext)
	return err
}

func (s *Store) SaveInstanceServerPassword(ctx context.Context, instanceID, ciphertext string) error {
	tag, err := s.db.ExecContext(ctx, `UPDATE instance_secrets SET server_password_ciphertext=NULLIF(?2,''),updated_at=unixepoch('subsec')*1000000 WHERE instance_id=?1`, instanceID, ciphertext)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) InstanceSecrets(ctx context.Context, instanceID string) (string, string, error) {
	var rconCiphertext, serverPasswordCiphertext string
	err := s.db.QueryRowContext(ctx, `SELECT rcon_ciphertext,COALESCE(server_password_ciphertext,'') FROM instance_secrets WHERE instance_id=?1`, instanceID).Scan(&rconCiphertext, &serverPasswordCiphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return rconCiphertext, serverPasswordCiphertext, err
}

func (s *Store) PublicServers(ctx context.Context) ([]domain.PublicServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance.id,instance.name,instance.status,
		CASE WHEN node.public_address<>'' THEN node.public_address || ':' || instance.port
			WHEN instance.host NOT IN ('','0.0.0.0','::','[::]') THEN instance.host || ':' || instance.port ELSE '' END,
		instance.map,COALESCE(NULLIF(instance.mode,''),NULLIF(json_extract(instance.config,'$.mode'),''),'unknown'),instance.players,instance.max_players,instance.started_at
		FROM instances instance JOIN nodes node ON node.id=instance.node_id
		WHERE COALESCE(CAST(json_extract(instance.config,'$.public_status') AS INTEGER),0)=1 ORDER BY instance.name,instance.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.PublicServer, 0)
	for rows.Next() {
		var item domain.PublicServer
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &item.Address, &item.Map, &item.Mode, &item.Players, &item.MaxPlayers, &item.StartedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func validateInstanceAction(action string) error {
	if _, ok := InstanceActionPermission(action); !ok || action == "create_schedule" {
		return fmt.Errorf("unsupported instance action %q", action)
	}
	return nil
}
