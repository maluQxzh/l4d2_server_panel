package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	raw, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().UTC().Add(ttl)
	_, err = s.pool.Exec(ctx, `INSERT INTO enrollment_tokens(id,token_hash,label,created_by,expires_at)
		VALUES($1,$2,$3,$4,$5)`, uuid.NewString(), tokenHash(raw), strings.TrimSpace(label), nullableUUID(createdBy), expires)
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
	if strings.TrimSpace(req.Token) == "" || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.InstanceRoot) == "" {
		return domain.Node{}, "", errors.New("token, name, and instance_root are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Node{}, "", err
	}
	defer tx.Rollback(ctx)
	var tokenID string
	err = tx.QueryRow(ctx, `SELECT id FROM enrollment_tokens WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now() FOR UPDATE`, tokenHash(req.Token)).Scan(&tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Node{}, "", ErrUnauthorized
	}
	if err != nil {
		return domain.Node{}, "", err
	}
	credential, err := randomToken(48)
	if err != nil {
		return domain.Node{}, "", err
	}
	node := domain.Node{
		ID: uuid.NewString(), Name: strings.TrimSpace(req.Name), Status: domain.NodeOffline,
		OS: strings.ToLower(strings.TrimSpace(req.OS)), Arch: strings.ToLower(strings.TrimSpace(req.Arch)),
		AgentVersion: strings.TrimSpace(req.AgentVersion), Address: remoteAddress,
		InstanceRoot: strings.TrimSpace(req.InstanceRoot), CreatedAt: time.Now().UTC(),
	}
	_, err = tx.Exec(ctx, `INSERT INTO nodes(id,name,status,os,arch,agent_version,address,instance_root,agent_credential_hash,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, node.ID, node.Name, node.Status, node.OS, node.Arch,
		node.AgentVersion, node.Address, node.InstanceRoot, tokenHash(credential), node.CreatedAt)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE enrollment_tokens SET consumed_at=now() WHERE id=$1`, tokenID)
	}
	if err != nil {
		return domain.Node{}, "", err
	}
	return node, credential, tx.Commit(ctx)
}

func (s *Store) NodeByCredential(ctx context.Context, credential string) (domain.Node, error) {
	n, err := scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE agent_credential_hash=$1 AND status<>'revoked'`, tokenHash(credential)))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Node{}, ErrUnauthorized
	}
	return n, err
}

func (s *Store) ListNodes(ctx context.Context) ([]domain.Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeResourceColumns+` FROM nodes ORDER BY created_at,id`)
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
	n, err := scanNodeResource(s.pool.QueryRow(ctx, `SELECT `+nodeResourceColumns+` FROM nodes WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Node{}, ErrNotFound
	}
	return n, err
}

func (s *Store) RenameNode(ctx context.Context, id, name string) (domain.Node, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return domain.Node{}, errors.New("node name must be between 1 and 128 characters")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE nodes SET name=$2 WHERE id=$1`, id, name)
	if err != nil {
		return domain.Node{}, err
	}
	if tag.RowsAffected() == 0 {
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
	tag, err := s.pool.Exec(ctx, `UPDATE nodes SET name=$2,public_address=COALESCE($3,public_address) WHERE id=$1`, id, name, normalized)
	if err != nil {
		return domain.Node{}, err
	}
	if tag.RowsAffected() == 0 {
		return domain.Node{}, ErrNotFound
	}
	return s.GetNode(ctx, id)
}

func (s *Store) RevokeNode(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE nodes SET status='revoked',agent_credential_hash=NULL WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) RotateNodeCredential(ctx context.Context, id string) (string, error) {
	credential, err := randomToken(48)
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE nodes SET agent_credential_hash=$2 WHERE id=$1 AND status<>'revoked'`, id, tokenHash(credential))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return credential, nil
}

func (s *Store) SetNodeConnected(ctx context.Context, id, address, version string) error {
	_, err := s.pool.Exec(ctx, `UPDATE nodes SET status='online',address=COALESCE(NULLIF($2,''),address),
		agent_version=COALESCE(NULLIF($3,''),agent_version),last_seen_at=now() WHERE id=$1`, id, address, version)
	return err
}

func (s *Store) SetNodeOffline(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE nodes SET status=CASE WHEN status='revoked' THEN status ELSE 'offline' END WHERE id=$1`, id)
	return err
}

func (s *Store) Housekeeping(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE expires_at<now()`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE requested_by IS NULL AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update')
		AND status IN ('succeeded','failed','canceled') AND NOT (result @> '{"_projection_pending":true}'::jsonb)
		AND finished_at<now()-interval '24 hours'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE type IN ('logs.search','logs.chunk','backup.chunk','backup.upload.chunk')
		AND status IN ('succeeded','failed','canceled') AND NOT (result @> '{"_projection_pending":true}'::jsonb)
		AND finished_at<now()-interval '24 hours'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE backup_uploads SET status='expired',updated_at=now()
		WHERE status IN ('uploading','ready') AND expires_at<=now()
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.type='backup.upload.chunk'
			AND j.payload->>'upload_id'=backup_uploads.id::text AND (
				j.status IN ('queued','dispatched','running') OR
				(j.status='succeeded' AND j.id IS DISTINCT FROM backup_uploads.last_chunk_job_id
					AND (j.payload->>'offset')::bigint=backup_uploads.received_size)))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM backup_uploads WHERE status IN ('expired','failed','succeeded')
		AND updated_at<now()-interval '30 days'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET
		payload=payload-ARRAY['command_ciphertext','source_ciphertext','content_ciphertext','content_base64_ciphertext','archive_ciphertext','keyword_ciphertext','existing_rcon_password_ciphertext'],
		result=result-ARRAY['response_ciphertext','content_base64_ciphertext','entries_ciphertext']
		WHERE status IN ('succeeded','failed','canceled') AND finished_at<now()-interval '24 hours'
		AND NOT (result @> '{"_projection_pending":true}'::jsonb)
		AND (payload ?| ARRAY['command_ciphertext','source_ciphertext','content_ciphertext','content_base64_ciphertext','archive_ciphertext','keyword_ciphertext','existing_rcon_password_ciphertext']
			OR result ?| ARRAY['response_ciphertext','content_base64_ciphertext','entries_ciphertext'])`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM event_log WHERE created_at<now()-interval '30 days'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM player_bans WHERE expires_at IS NOT NULL AND expires_at<=now()`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM quarantine_purge_confirmations
		WHERE expires_at<now()-interval '24 hours' OR consumed_at<now()-interval '24 hours'`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) MarkStaleNodes(ctx context.Context) ([]domain.Node, error) {
	rows, err := s.pool.Query(ctx, `UPDATE nodes SET status='offline' WHERE status='online' AND last_seen_at<now()-interval '35 seconds'
		RETURNING `+nodeColumns)
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE nodes SET status='online',agent_version=$2,last_seen_at=now() WHERE id=$1 AND status<>'revoked'`, nodeID, agentVersion); err != nil {
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
		return tx.Commit(ctx)
	}
	statusJSON, err := json.Marshal(validStatuses)
	if err != nil {
		return err
	}

	// Lock every reported instance before taking the statement snapshot used by
	// the active-job EXISTS below. QueueInstanceJob takes a conflicting row lock
	// before publishing a job, so a concurrent queue either precedes this fresh
	// snapshot or follows the heartbeat commit. The deterministic ordering also
	// prevents a multi-instance heartbeat from deadlocking another heartbeat.
	lockedRows, err := tx.Query(ctx, `SELECT instance.id
		FROM instances AS instance
		JOIN jsonb_each_text($2::jsonb) AS incoming(instance_id,status)
			ON instance.id=incoming.instance_id::uuid
		WHERE instance.node_id=$1
		ORDER BY instance.id
		FOR UPDATE OF instance`, nodeID, string(statusJSON))
	if err != nil {
		return err
	}
	for lockedRows.Next() {
	}
	if err := lockedRows.Err(); err != nil {
		lockedRows.Close()
		return err
	}
	lockedRows.Close()

	// Heartbeats report process reality, but the Control-side lifecycle status
	// remains authoritative while a write job owns the instance. Node health is
	// still refreshed above and metric/A2S projections remain independent. Once
	// the terminal projector clears its durable marker, the next heartbeat can
	// publish the Agent's stable running/stopped state again.
	if _, err := tx.Exec(ctx, `UPDATE instances AS instance SET
		status=CASE WHEN EXISTS (
			SELECT 1 FROM jobs AS job WHERE job.instance_id=instance.id
				AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`
		) THEN instance.status ELSE incoming.status END,
		updated_at=now()
		FROM jsonb_each_text($2::jsonb) AS incoming(instance_id,status)
		WHERE instance.id=incoming.instance_id::uuid AND instance.node_id=$1`, nodeID, string(statusJSON)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SaveMetric(ctx context.Context, sample domain.MetricSample) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO metrics(node_id,instance_id,cpu_percent,memory_used,memory_total,disk_used,disk_total,
		network_rx,network_tx,players,collected_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		sample.NodeID, nullableUUID(sample.InstanceID), sample.CPUPercent, int64(sample.MemoryUsed), int64(sample.MemoryTotal),
		int64(sample.DiskUsed), int64(sample.DiskTotal), int64(sample.NetworkRx), int64(sample.NetworkTx), sample.Players, sample.CollectedAt)
	if err == nil && sample.InstanceID == "" {
		_, err = s.pool.Exec(ctx, `UPDATE nodes SET cpu_percent=$2,memory_used=$3,memory_total=$4,disk_used=$5,disk_total=$6,last_seen_at=now()
			WHERE id=$1`, sample.NodeID, sample.CPUPercent, int64(sample.MemoryUsed), int64(sample.MemoryTotal), int64(sample.DiskUsed), int64(sample.DiskTotal))
	}
	return err
}

func nullableUUID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

const instanceColumns = `id,node_id,name,status,path,host,port,tv_port,pid,map,mode,players,max_players,build_id,
		version,config,started_at,created_at,updated_at,
		COALESCE((SELECT public_address FROM nodes WHERE nodes.id=instances.node_id),''),
		COALESCE((SELECT state.available FROM instance_update_state state WHERE state.instance_id=instances.id),false),
		COALESCE((SELECT state.available_build FROM instance_update_state state WHERE state.instance_id=instances.id),''),
		(SELECT state.checked_at FROM instance_update_state state WHERE state.instance_id=instances.id)`

func scanInstance(row scanner) (domain.Instance, error) {
	var inst domain.Instance
	var raw []byte
	err := row.Scan(&inst.ID, &inst.NodeID, &inst.Name, &inst.Status, &inst.Path, &inst.Host, &inst.Port, &inst.TVPort,
		&inst.PID, &inst.Map, &inst.Mode, &inst.Players, &inst.MaxPlayers, &inst.BuildID, &inst.Version, &raw,
		&inst.StartedAt, &inst.CreatedAt, &inst.UpdatedAt, &inst.NodePublicAddress, &inst.UpdateAvailable,
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
		query += ` WHERE node_id=$1`
		args = append(args, nodeID)
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.pool.Query(ctx, query, args...)
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
	inst, err := scanInstance(s.pool.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Instance{}, ErrNotFound
	}
	return inst, err
}

type CreateInstanceInput struct {
	NodeID string
	Name   string
	Path   string
	Host   string
	Port   int
	TVPort int
	Config domain.InstanceConfig
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
	config, err := json.Marshal(input.Config)
	if err != nil {
		return domain.Instance{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Instance{}, err
	}
	defer tx.Rollback(ctx)
	// Serializing instance creation per node makes automatic selection stable:
	// concurrent requests cannot both observe the same candidate before the
	// reservation trigger records it. Different nodes remain independent.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, input.NodeID); err != nil {
		return domain.Instance{}, err
	}
	if input.Port == 0 {
		err := tx.QueryRow(ctx, `SELECT candidate FROM generate_series(27015,27999) candidate
			WHERE ($2=0 OR candidate<>$2)
			AND NOT EXISTS (SELECT 1 FROM instance_port_reservations WHERE node_id=$1 AND port=candidate)
			ORDER BY candidate LIMIT 1`, input.NodeID, input.TVPort).Scan(&input.Port)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Instance{}, fmt.Errorf("no available game port on node: %w", ErrConflict)
		}
		if err != nil {
			return domain.Instance{}, fmt.Errorf("allocate port: %w", err)
		}
	}
	inst := domain.Instance{ID: uuid.NewString(), NodeID: input.NodeID, Name: strings.TrimSpace(input.Name), Status: domain.InstanceInstalling,
		Path: input.Path, Host: input.Host, Port: input.Port, TVPort: input.TVPort, MaxPlayers: input.Config.MaxPlayers,
		Version: 1, Config: input.Config, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err = tx.Exec(ctx, `INSERT INTO instances(id,node_id,name,status,path,host,port,tv_port,max_players,version,config,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, inst.ID, inst.NodeID, inst.Name, inst.Status, inst.Path,
		inst.Host, inst.Port, inst.TVPort, inst.MaxPlayers, inst.Version, config, inst.CreatedAt, inst.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23503") {
			return domain.Instance{}, ErrConflict
		}
		return domain.Instance{}, err
	}
	if err := tx.Commit(ctx); err != nil {
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

func (s *Store) UpdateInstance(ctx context.Context, id, name string, config *domain.InstanceConfig, expectedVersion int, userID string) (domain.Instance, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Instance{}, err
	}
	defer tx.Rollback(ctx)
	current, err := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
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
		previous, _ := json.Marshal(current.Config)
		_, err = tx.Exec(ctx, `INSERT INTO config_versions(id,instance_id,version,config,created_by) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(instance_id,version) DO NOTHING`, uuid.NewString(), id, current.Version, previous, nullableUUID(userID))
		if err != nil {
			return domain.Instance{}, err
		}
		current.Config = *config
		current.MaxPlayers = config.MaxPlayers
	}
	raw, _ := json.Marshal(current.Config)
	current.Version++
	current.UpdatedAt = time.Now().UTC()
	_, err = tx.Exec(ctx, `UPDATE instances SET name=$2,config=$3,max_players=$4,version=$5,updated_at=$6 WHERE id=$1`,
		id, current.Name, raw, current.MaxPlayers, current.Version, current.UpdatedAt)
	if err != nil {
		return domain.Instance{}, err
	}
	return current, tx.Commit(ctx)
}

func (s *Store) UpdateInstanceAndQueue(ctx context.Context, id, name string, config *domain.InstanceConfig, expectedVersion int,
	userID, idempotencyKey string, serverPasswordCiphertext *string) (domain.Instance, domain.Job, bool, error) {
	request := map[string]any{"name": name, "config": config, "version": expectedVersion}
	if serverPasswordCiphertext != nil {
		// This compatibility path cannot recover the plaintext after encryption.
		// HTTP handlers use UpdateInstanceAndQueueWithFingerprint and hash the
		// clear request before encryption; direct callers still fail safely on a
		// changed ciphertext.
		request["server_password_ciphertext"] = *serverPasswordCiphertext
	}
	requestFingerprint, err := canonicalRequestFingerprint("instance-config", id, "configure", request)
	if err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	return s.UpdateInstanceAndQueueWithFingerprint(ctx, id, name, config, expectedVersion, userID, idempotencyKey,
		serverPasswordCiphertext, requestFingerprint)
}

func (s *Store) UpdateInstanceAndQueueWithFingerprint(ctx context.Context, id, name string, config *domain.InstanceConfig, expectedVersion int,
	userID, idempotencyKey string, serverPasswordCiphertext *string, requestFingerprint string) (domain.Instance, domain.Job, bool, error) {
	if len(idempotencyKey) > 200 {
		return domain.Instance{}, domain.Job{}, false, errors.New("idempotency key is too long")
	}
	if err := requireRequestFingerprint(idempotencyKey, requestFingerprint); err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	defer tx.Rollback(ctx)
	if idempotencyKey != "" {
		existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRow(ctx, `SELECT `+jobColumnsWithFingerprint+` FROM jobs WHERE requested_by=$1 AND idempotency_key=$2`, userID, idempotencyKey))
		if lookupErr == nil {
			if !requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
				return domain.Instance{}, domain.Job{}, false, ErrConflict
			}
			current, getErr := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=$1`, id))
			if getErr != nil {
				return domain.Instance{}, domain.Job{}, false, getErr
			}
			return current, existing, true, tx.Commit(ctx)
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.Instance{}, domain.Job{}, false, lookupErr
		}
	}
	current, err := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Instance{}, domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	if current.Status == domain.InstanceExternal {
		return domain.Instance{}, domain.Job{}, false, fmt.Errorf("external instance is read-only until a controlled restart: %w", ErrConflict)
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE instance_id=$1
		AND (status IN ('queued','dispatched','running')
			OR (status='canceled' AND result @> '{"cancel_pending":true}'::jsonb)
			OR (status IN ('succeeded','failed') AND result @> '{"_projection_pending":true}'::jsonb))
		AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort')
		AND NOT (type='file' AND payload->>'operation'='read'))`, id).Scan(&busy); err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	if busy {
		return domain.Instance{}, domain.Job{}, false, ErrConflict
	}
	if expectedVersion > 0 && current.Version != expectedVersion {
		return domain.Instance{}, domain.Job{}, false, ErrConflict
	}
	if strings.TrimSpace(name) != "" {
		current.Name = strings.TrimSpace(name)
	}
	if config != nil {
		previous, _ := json.Marshal(current.Config)
		if _, err := tx.Exec(ctx, `INSERT INTO config_versions(id,instance_id,version,config,created_by) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(instance_id,version) DO NOTHING`, uuid.NewString(), id, current.Version, previous, nullableUUID(userID)); err != nil {
			return domain.Instance{}, domain.Job{}, false, err
		}
		config.ServerPassword = ""
		current.Config = *config
		current.MaxPlayers = config.MaxPlayers
	}
	if serverPasswordCiphertext != nil {
		tag, err := tx.Exec(ctx, `UPDATE instance_secrets SET server_password_ciphertext=NULLIF($2,''),updated_at=now() WHERE instance_id=$1`, id, *serverPasswordCiphertext)
		if err != nil {
			return domain.Instance{}, domain.Job{}, false, err
		}
		if tag.RowsAffected() == 0 {
			return domain.Instance{}, domain.Job{}, false, ErrNotFound
		}
	}
	rawConfig, _ := json.Marshal(current.Config)
	current.Version++
	current.UpdatedAt = time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE instances SET name=$2,config=$3,max_players=$4,version=$5,updated_at=$6 WHERE id=$1`,
		id, current.Name, rawConfig, current.MaxPlayers, current.Version, current.UpdatedAt); err != nil {
		return domain.Instance{}, domain.Job{}, false, err
	}
	payload := map[string]any{"instance": current}
	rawPayload, _ := json.Marshal(payload)
	job := domain.Job{ID: uuid.NewString(), NodeID: current.NodeID, InstanceID: current.ID, Type: "configure", Status: domain.JobQueued,
		Payload: payload, IdempotencyKey: idempotencyKey, RequestedBy: userID, CreatedAt: time.Now().UTC()}
	if _, err := tx.Exec(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,NULLIF($10,''))`, job.ID, job.NodeID, job.InstanceID, job.Type, job.Status,
		rawPayload, job.IdempotencyKey, job.RequestedBy, job.CreatedAt, requestFingerprint); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate key") {
			return domain.Instance{}, domain.Job{}, false, ErrConflict
		}
		return domain.Instance{}, domain.Job{}, false, err
	}
	return current, job, false, tx.Commit(ctx)
}

func (s *Store) RemoveInstanceRecord(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM instances WHERE id=$1 AND NOT EXISTS (
		SELECT 1 FROM jobs WHERE instance_id=$1
		AND (status IN ('queued','dispatched','running')
			OR (status='canceled' AND result @> '{"cancel_pending":true}'::jsonb)
			OR (status IN ('succeeded','failed') AND result @> '{"_projection_pending":true}'::jsonb))
		AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort')
		AND NOT (type='file' AND payload->>'operation'='read'))`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

func (s *Store) DeleteInstanceAfterJob(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM instances WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SaveInstanceSecrets(ctx context.Context, instanceID, rconCiphertext, serverPasswordCiphertext string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO instance_secrets(instance_id,rcon_ciphertext,server_password_ciphertext) VALUES($1,$2,NULLIF($3,''))
		ON CONFLICT(instance_id) DO UPDATE SET rcon_ciphertext=EXCLUDED.rcon_ciphertext,
		server_password_ciphertext=EXCLUDED.server_password_ciphertext,updated_at=now()`, instanceID, rconCiphertext, serverPasswordCiphertext)
	return err
}

func (s *Store) SaveInstanceServerPassword(ctx context.Context, instanceID, ciphertext string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE instance_secrets SET server_password_ciphertext=NULLIF($2,''),updated_at=now() WHERE instance_id=$1`, instanceID, ciphertext)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) InstanceSecrets(ctx context.Context, instanceID string) (string, string, error) {
	var rconCiphertext, serverPasswordCiphertext string
	err := s.pool.QueryRow(ctx, `SELECT rcon_ciphertext,COALESCE(server_password_ciphertext,'') FROM instance_secrets WHERE instance_id=$1`, instanceID).Scan(&rconCiphertext, &serverPasswordCiphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return rconCiphertext, serverPasswordCiphertext, err
}

func (s *Store) PublicServers(ctx context.Context) ([]domain.PublicServer, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance.id,instance.name,instance.status,
		CASE WHEN node.public_address<>'' THEN node.public_address || ':' || instance.port::text
			WHEN instance.host NOT IN ('','0.0.0.0','::','[::]') THEN instance.host || ':' || instance.port::text ELSE '' END,
		instance.map,COALESCE(NULLIF(instance.mode,''),NULLIF(instance.config->>'mode',''),'unknown'),instance.players,instance.max_players,instance.started_at
		FROM instances instance JOIN nodes node ON node.id=instance.node_id
		WHERE COALESCE((instance.config->>'public_status')::boolean,false)=true ORDER BY instance.name,instance.id`)
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
