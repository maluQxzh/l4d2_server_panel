package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	metricResolutionRaw    = "raw"
	metricResolutionMinute = "1m"
	metricResolutionHour   = "1h"
)

// MetricPoint is shared by raw and rolled-up responses. SampleCount is one for
// raw samples and records the number of underlying raw samples for rollups.
type MetricPoint struct {
	NodeID      string    `json:"node_id"`
	InstanceID  string    `json:"instance_id,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
	CPUPercent  float64   `json:"cpu_percent"`
	MemoryUsed  int64     `json:"memory_used"`
	MemoryTotal int64     `json:"memory_total"`
	DiskUsed    int64     `json:"disk_used"`
	DiskTotal   int64     `json:"disk_total"`
	NetworkRx   int64     `json:"network_rx"`
	NetworkTx   int64     `json:"network_tx"`
	Players     int       `json:"players"`
	SampleCount int       `json:"sample_count"`
}

type metricQuery struct {
	NodeID     string
	InstanceID string
	Resolution string
	From       time.Time
	To         time.Time
}

func parseMetricQuery(values url.Values, now time.Time) (metricQuery, error) {
	query := metricQuery{
		NodeID: strings.TrimSpace(values.Get("node_id")), InstanceID: strings.TrimSpace(values.Get("instance_id")),
		Resolution: strings.ToLower(strings.TrimSpace(values.Get("resolution"))), To: now.UTC(),
	}
	if query.Resolution == "" {
		query.Resolution = metricResolutionMinute
	}
	if query.NodeID == "" && query.InstanceID == "" {
		return metricQuery{}, errors.New("node_id or instance_id is required")
	}
	if query.NodeID != "" {
		if _, err := uuid.Parse(query.NodeID); err != nil {
			return metricQuery{}, errors.New("node_id must be a UUID")
		}
	}
	if query.InstanceID != "" {
		if _, err := uuid.Parse(query.InstanceID); err != nil {
			return metricQuery{}, errors.New("instance_id must be a UUID")
		}
	}
	var defaultWindow, maximumWindow time.Duration
	switch query.Resolution {
	case metricResolutionRaw:
		defaultWindow, maximumWindow = time.Hour, 24*time.Hour
	case metricResolutionMinute:
		defaultWindow, maximumWindow = 24*time.Hour, 31*24*time.Hour
	case metricResolutionHour:
		defaultWindow, maximumWindow = 30*24*time.Hour, 366*24*time.Hour
	default:
		return metricQuery{}, errors.New("resolution must be raw, 1m, or 1h")
	}
	var err error
	if raw := strings.TrimSpace(values.Get("to")); raw != "" {
		query.To, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return metricQuery{}, errors.New("to must be RFC3339")
		}
		query.To = query.To.UTC()
	}
	query.From = query.To.Add(-defaultWindow)
	if raw := strings.TrimSpace(values.Get("from")); raw != "" {
		query.From, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return metricQuery{}, errors.New("from must be RFC3339")
		}
		query.From = query.From.UTC()
	}
	if !query.From.Before(query.To) {
		return metricQuery{}, errors.New("from must be before to")
	}
	if query.To.Sub(query.From) > maximumWindow {
		return metricQuery{}, fmt.Errorf("requested range is too large for %s resolution", query.Resolution)
	}
	return query, nil
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	query, err := parseMetricQuery(r.URL.Query(), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_metrics_query", err.Error())
		return
	}
	principal := principalFromContext(r.Context())
	if principal.HasResourceRestriction() {
		if query.InstanceID != "" {
			instance, lookupErr := s.store.GetInstance(r.Context(), query.InstanceID)
			if lookupErr != nil {
				s.fail(w, lookupErr)
				return
			}
			if !principal.AllowsInstancePermission(instance, PermissionRead) || (query.NodeID != "" && query.NodeID != instance.NodeID) {
				writeTokenScopeDenied(w)
				return
			}
		} else if !principal.AllowsNodePermission(query.NodeID, PermissionRead) {
			writeTokenScopeDenied(w)
			return
		}
	}
	items, err := s.store.Metrics(r.Context(), query)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"resolution": query.Resolution,
		"from":       query.From,
		"to":         query.To,
	})
}

func (s *Store) Metrics(ctx context.Context, query metricQuery) ([]MetricPoint, error) {
	table, timestamp, sampleCount := "metrics", "collected_at", "1"
	if query.Resolution == metricResolutionMinute {
		table, timestamp, sampleCount = "metrics_1m", "bucket", "sample_count"
	} else if query.Resolution == metricResolutionHour {
		table, timestamp, sampleCount = "metrics_1h", "bucket", "sample_count"
	}
	// table/timestamp/sampleCount are selected only from the closed enum above;
	// all caller-provided values remain parameters.
	statement := `SELECT node_id::text,COALESCE(instance_id::text,''),` + timestamp + `,
		cpu_percent,memory_used,memory_total,disk_used,disk_total,network_rx,network_tx,players,` + sampleCount + `
		FROM ` + table + ` WHERE ($1::uuid IS NULL OR node_id=$1)
		AND ($2::uuid IS NULL OR instance_id=$2) AND ` + timestamp + ` >= $3 AND ` + timestamp + ` <= $4
		ORDER BY ` + timestamp + `,node_id,instance_id NULLS FIRST LIMIT 10000`
	rows, err := s.pool.Query(ctx, statement, nullableUUID(query.NodeID), nullableUUID(query.InstanceID), query.From, query.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]MetricPoint, 0)
	for rows.Next() {
		var item MetricPoint
		if err := rows.Scan(&item.NodeID, &item.InstanceID, &item.CollectedAt, &item.CPUPercent, &item.MemoryUsed,
			&item.MemoryTotal, &item.DiskUsed, &item.DiskTotal, &item.NetworkRx, &item.NetworkTx, &item.Players, &item.SampleCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// AggregateMetrics incrementally revisits only the newest rollup window. The
// overlap makes late samples and the currently-open bucket deterministic while
// avoiding a full history scan every 30 seconds.
func (s *Store) AggregateMetrics(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `WITH boundary AS (
		SELECT COALESCE(max(bucket)-interval '2 minutes',now()-interval '30 days') AS from_time FROM metrics_1m
	), source AS (
		SELECT node_id,instance_id,date_trunc('minute',collected_at) AS bucket,
			avg(cpu_percent) AS cpu_percent,avg(memory_used)::bigint AS memory_used,max(memory_total) AS memory_total,
			avg(disk_used)::bigint AS disk_used,max(disk_total) AS disk_total,max(network_rx) AS network_rx,
			max(network_tx) AS network_tx,avg(players)::integer AS players,count(*)::integer AS sample_count
		FROM metrics,boundary WHERE collected_at>=boundary.from_time
		GROUP BY node_id,instance_id,date_trunc('minute',collected_at)
	)
	INSERT INTO metrics_1m(node_id,instance_id,bucket,cpu_percent,memory_used,memory_total,disk_used,disk_total,
		network_rx,network_tx,players,sample_count)
	SELECT node_id,instance_id,bucket,cpu_percent,memory_used,memory_total,disk_used,disk_total,
		network_rx,network_tx,players,sample_count FROM source
	ON CONFLICT(node_id,instance_id,bucket) DO UPDATE SET
		cpu_percent=EXCLUDED.cpu_percent,memory_used=EXCLUDED.memory_used,memory_total=EXCLUDED.memory_total,
		disk_used=EXCLUDED.disk_used,disk_total=EXCLUDED.disk_total,network_rx=EXCLUDED.network_rx,
		network_tx=EXCLUDED.network_tx,players=EXCLUDED.players,sample_count=EXCLUDED.sample_count`); err != nil {
		return fmt.Errorf("aggregate one-minute metrics: %w", err)
	}
	if _, err = tx.Exec(ctx, `WITH boundary AS (
		SELECT COALESCE(max(bucket)-interval '2 hours',now()-interval '1 year') AS from_time FROM metrics_1h
	), source AS (
		SELECT node_id,instance_id,date_trunc('hour',bucket) AS bucket,
			(sum(cpu_percent*sample_count)/NULLIF(sum(sample_count),0))::double precision AS cpu_percent,
			(sum(memory_used*sample_count)/NULLIF(sum(sample_count),0))::bigint AS memory_used,max(memory_total) AS memory_total,
			(sum(disk_used*sample_count)/NULLIF(sum(sample_count),0))::bigint AS disk_used,max(disk_total) AS disk_total,
			max(network_rx) AS network_rx,max(network_tx) AS network_tx,
			(sum(players*sample_count)/NULLIF(sum(sample_count),0))::integer AS players,sum(sample_count)::integer AS sample_count
		FROM metrics_1m,boundary WHERE bucket>=boundary.from_time
		GROUP BY node_id,instance_id,date_trunc('hour',bucket)
	)
	INSERT INTO metrics_1h(node_id,instance_id,bucket,cpu_percent,memory_used,memory_total,disk_used,disk_total,
		network_rx,network_tx,players,sample_count)
	SELECT node_id,instance_id,bucket,cpu_percent,memory_used,memory_total,disk_used,disk_total,
		network_rx,network_tx,players,sample_count FROM source
	ON CONFLICT(node_id,instance_id,bucket) DO UPDATE SET
		cpu_percent=EXCLUDED.cpu_percent,memory_used=EXCLUDED.memory_used,memory_total=EXCLUDED.memory_total,
		disk_used=EXCLUDED.disk_used,disk_total=EXCLUDED.disk_total,network_rx=EXCLUDED.network_rx,
		network_tx=EXCLUDED.network_tx,players=EXCLUDED.players,sample_count=EXCLUDED.sample_count`); err != nil {
		return fmt.Errorf("aggregate one-hour metrics: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM metrics WHERE collected_at<now()-interval '24 hours'`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM metrics_1m WHERE bucket<now()-interval '30 days'`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM metrics_1h WHERE bucket<now()-interval '1 year'`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type monitoringTarget struct {
	InstanceID string
	NodeID     string
}

func (s *Store) RunningMonitoringTargets(ctx context.Context) ([]monitoringTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance.id::text,instance.node_id::text FROM instances instance
		JOIN nodes node ON node.id=instance.node_id WHERE instance.status='running' AND node.status='online'
		ORDER BY instance.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]monitoringTarget, 0)
	for rows.Next() {
		var target monitoringTarget
		if err := rows.Scan(&target.InstanceID, &target.NodeID); err != nil {
			return nil, err
		}
		items = append(items, target)
	}
	return items, rows.Err()
}

func (s *Store) UpdateMonitoringTargets(ctx context.Context) ([]monitoringTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance.id::text,instance.node_id::text FROM instances instance
		JOIN nodes node ON node.id=instance.node_id WHERE node.status='online'
		AND instance.status IN ('stopped','running','degraded')
		AND NOT EXISTS (
			SELECT 1 FROM jobs job WHERE job.instance_id=instance.id
			AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`
		) ORDER BY instance.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]monitoringTarget, 0)
	for rows.Next() {
		var target monitoringTarget
		if err := rows.Scan(&target.InstanceID, &target.NodeID); err != nil {
			return nil, err
		}
		items = append(items, target)
	}
	return items, rows.Err()
}

// QueueSystemProbe is intentionally closed to read-only monitoring commands.
// It bypasses user-facing action validation only so capability-gated future
// probes cannot accidentally become a public arbitrary-job endpoint.
func (s *Store) QueueSystemProbe(ctx context.Context, target monitoringTarget, jobType, key string) (domain.Job, bool, error) {
	if jobType != "a2s.info" && jobType != "check_update" {
		return domain.Job{}, false, errors.New("unsupported system probe")
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: target.NodeID, InstanceID: target.InstanceID, Type: jobType,
		Status: domain.JobQueued, Payload: map[string]any{}, IdempotencyKey: key, CreatedAt: time.Now().UTC()}
	requestFingerprint, err := instanceJobRequestFingerprint(target.InstanceID, jobType, job.Payload)
	if err != nil {
		return domain.Job{}, false, err
	}
	if err := requireRequestFingerprint(key, requestFingerprint); err != nil {
		return domain.Job{}, false, err
	}
	payload, _ := json.Marshal(job.Payload)
	if jobType == "check_update" {
		return s.queueUpdateProbe(ctx, target, job, payload, requestFingerprint)
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,NULL,$8,$9) ON CONFLICT DO NOTHING RETURNING created_at`,
		job.ID, job.NodeID, job.InstanceID, job.Type, job.Status, payload, key, job.CreatedAt, requestFingerprint).Scan(&job.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, lookupErr := s.JobByIdempotencyKeyMatching(ctx, "", key, requestFingerprint)
		if lookupErr != nil {
			return domain.Job{}, false, lookupErr
		}
		return existing, true, nil
	}
	return job, false, err
}

// queueUpdateProbe uses the same instance row lock as automatic maintenance
// batching and check_update dispatch. This closes the MVCC gap where a probe
// could otherwise be inserted/claimed after a batch's active-job check but
// before its update intent commits.
func (s *Store) queueUpdateProbe(ctx context.Context, target monitoringTarget, job domain.Job, payload []byte, requestFingerprint string) (domain.Job, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Job{}, false, err
	}
	defer tx.Rollback(ctx)
	var nodeID string
	if err := tx.QueryRow(ctx, `SELECT node_id::text FROM instances WHERE id=$1 FOR UPDATE`, target.InstanceID).Scan(&nodeID); errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	} else if err != nil {
		return domain.Job{}, false, err
	} else if nodeID != target.NodeID {
		return domain.Job{}, false, ErrConflict
	}

	existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRow(ctx, `SELECT `+jobColumnsWithFingerprint+`
		FROM jobs WHERE requested_by IS NULL AND idempotency_key=$1`, job.IdempotencyKey))
	if lookupErr == nil {
		if existing.NodeID != target.NodeID || existing.InstanceID != target.InstanceID || existing.Type != "check_update" ||
			!requestFingerprintsEqual(storedFingerprint, requestFingerprint) {
			return domain.Job{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Job{}, false, err
		}
		return existing, true, nil
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return domain.Job{}, false, lookupErr
	}
	var conflicting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs job WHERE job.instance_id=$1
		AND `+activeJobSQLPredicate+` AND (`+instanceWriteJobSQLPredicate+` OR job.type='check_update'))`, target.InstanceID).Scan(&conflicting); err != nil {
		return domain.Job{}, false, err
	}
	if conflicting {
		return domain.Job{}, false, ErrConflict
	}
	if err := tx.QueryRow(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,NULL,$8,$9) ON CONFLICT DO NOTHING RETURNING created_at`,
		job.ID, job.NodeID, job.InstanceID, job.Type, job.Status, payload, job.IdempotencyKey, job.CreatedAt, requestFingerprint).Scan(&job.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, ErrConflict
	} else if err != nil {
		return domain.Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Job{}, false, err
	}
	return job, false, nil
}

func (s *Server) runMonitoringProbes(ctx context.Context) {
	targets, err := s.store.RunningMonitoringTargets(ctx)
	if err != nil {
		s.logger.Error("load monitoring targets", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, target := range targets {
		key := fmt.Sprintf("monitor:a2s:%s:%d", target.InstanceID, now.Unix()/30)
		job, reused, queueErr := s.store.QueueSystemProbe(ctx, target, "a2s.info", key)
		if queueErr != nil {
			s.logger.Error("queue A2S probe", "instance_id", target.InstanceID, "error", queueErr)
		} else if !reused {
			s.dispatchSoon(job.NodeID)
		}
	}
	updateTargets, err := s.store.UpdateMonitoringTargets(ctx)
	if err != nil {
		s.logger.Error("load update monitoring targets", "error", err)
		return
	}
	for _, target := range updateTargets {
		// Previous compatible Agents did not implement this command. Capability
		// gating prevents the control plane from knowingly dispatching a probe
		// that the connected Agent would reject as unsupported.
		if !s.agents.Supports(target.NodeID, "check_update") {
			continue
		}
		key := updateProbeKey(target.InstanceID, now)
		job, reused, queueErr := s.store.QueueSystemProbe(ctx, target, "check_update", key)
		if queueErr != nil && !errors.Is(queueErr, ErrConflict) {
			s.logger.Error("queue game update probe", "instance_id", target.InstanceID, "error", queueErr)
		} else if !reused {
			s.dispatchSoon(job.NodeID)
		}
	}
}

func updateProbeKey(instanceID string, at time.Time) string {
	return fmt.Sprintf("monitor:update:%s:%d", instanceID, at.UTC().Unix()/int64((30*time.Minute)/time.Second))
}

func (s *Store) ApplyUpdateCheckResult(ctx context.Context, job domain.Job) error {
	if job.Type != "check_update" || job.Status != domain.JobSucceeded || job.InstanceID == "" {
		return nil
	}
	available, _ := job.Result["available"].(bool)
	installed := stringFromAny(job.Result["installed_build"])
	availableBuild := stringFromAny(job.Result["available_build"])
	// A check queued before a later update may finish after that update. Never
	// let its stale public-branch comparison resurrect update_available; the
	// next probe, which dispatch gating holds until the write finishes, will
	// observe the newly installed build.
	tag, err := s.pool.Exec(ctx, `INSERT INTO instance_update_state(instance_id,installed_build,available_build,available,checked_at)
		SELECT $1,$2,$3,$4,$5 WHERE NOT EXISTS (
			SELECT 1 FROM jobs newer WHERE newer.instance_id=$1
			AND newer.type IN ('install','reinstall','update','validate') AND newer.created_at>$6
		) ON CONFLICT(instance_id) DO UPDATE SET installed_build=EXCLUDED.installed_build,
		available_build=EXCLUDED.available_build,available=EXCLUDED.available,checked_at=EXCLUDED.checked_at`,
		job.InstanceID, installed, availableBuild, available, jobProjectionTimestamp(job), job.CreatedAt)
	if err != nil || !available || tag.RowsAffected() == 0 {
		return err
	}
	return s.CreateNotificationOnce(ctx, "info", "update.available", "Game update available",
		"A newer L4D2 dedicated server build is available", job.InstanceID,
		map[string]any{"instance_id": job.InstanceID, "installed_build": installed, "available_build": availableBuild}, 6*time.Hour)
}

func (s *Store) clearAvailableUpdateAfterInstall(ctx context.Context, instanceID, installedBuild string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO instance_update_state(instance_id,installed_build,available_build,available,checked_at)
		VALUES($1,$2,$2,false,now()) ON CONFLICT(instance_id) DO UPDATE SET
		installed_build=COALESCE(NULLIF(EXCLUDED.installed_build,''),instance_update_state.installed_build),
		available_build=COALESCE(NULLIF(EXCLUDED.available_build,''),instance_update_state.available_build),
		available=false,checked_at=EXCLUDED.checked_at`, instanceID, installedBuild)
	return err
}
