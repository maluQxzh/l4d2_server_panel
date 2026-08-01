package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/robfig/cron/v3"
)

const (
	maintenanceUpdateKeyPrefix = "maintenance:update:"
	maintenanceCatchUpWindow   = 5 * time.Minute
)

var (
	automaticMaintenanceWarningSeconds = []int{600, 300, 60}
	fiveFieldCronParser                = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
)

// These predicates mirror the per-instance write exclusion index. Maintenance
// selection, monitoring, and dispatch must agree that cancellation and
// terminal projection outboxes still own the instance.
const activeJobSQLPredicate = `(job.status IN ('queued','dispatched','running')
	OR (job.status='canceled' AND json_extract(job.result,'$.cancel_pending')=1)
	OR (job.status IN ('succeeded','failed') AND json_extract(job.result,'$._projection_pending')=1))`

const instanceWriteJobSQLPredicate = `(job.type NOT IN (
	'a2s.info','a2s.players','rcon.players','list','check_update',
	'logs.search','logs.chunk','backup.chunk','backup.upload.abort'
)
	AND NOT (job.type='file' AND json_extract(job.payload,'$.operation')='read'))`

type maintenanceUpdateCandidate struct {
	Instance domain.Instance
	Cron     string
}

type queuedMaintenanceUpdate struct {
	Job    domain.Job
	Reused bool
}

type maintenanceBatchResult struct {
	Blocked bool
	Jobs    []queuedMaintenanceUpdate
}

// parseFiveFieldCron rejects descriptors, CRON_TZ prefixes, and seconds. Those
// forms would either bypass the configured global timezone or violate the
// documented standard five-field maintenance contract.
func parseFiveFieldCron(expression string) (cron.Schedule, error) {
	expression = strings.TrimSpace(expression)
	if len(strings.Fields(expression)) != 5 {
		return nil, errors.New("cron expression must contain exactly five fields")
	}
	return fiveFieldCronParser.Parse(expression)
}

// maintenanceWindowOccurrence finds the latest due wall-clock occurrence in
// the bounded startup catch-up interval. The lower boundary is inclusive.
func maintenanceWindowOccurrence(expression string, location *time.Location, now time.Time) (time.Time, bool, error) {
	if location == nil {
		return time.Time{}, false, errors.New("maintenance timezone is required")
	}
	schedule, err := parseFiveFieldCron(expression)
	if err != nil {
		return time.Time{}, false, err
	}
	localNow := now.In(location)
	boundary := localNow.Add(-maintenanceCatchUpWindow)
	next := schedule.Next(boundary.Add(-time.Nanosecond))
	if next.After(localNow) {
		return time.Time{}, false, nil
	}
	latest := next
	for {
		next = schedule.Next(latest)
		if next.After(localNow) {
			break
		}
		latest = next
	}
	if latest.Before(boundary) {
		return time.Time{}, false, nil
	}
	return latest.UTC(), true, nil
}

func maintenanceUpdateKey(windowStart time.Time, instanceID string) string {
	return maintenanceUpdateKeyPrefix + strconv.FormatInt(windowStart.UTC().Unix(), 10) + ":" + instanceID
}

func parseMaintenanceUpdateKey(key string) (time.Time, string, bool) {
	if !strings.HasPrefix(key, maintenanceUpdateKeyPrefix) {
		return time.Time{}, "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, maintenanceUpdateKeyPrefix), ":")
	if len(parts) != 2 {
		return time.Time{}, "", false
	}
	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, "", false
	}
	parsedID, err := uuid.Parse(parts[1])
	if err != nil || parsedID.String() != strings.ToLower(parts[1]) {
		return time.Time{}, "", false
	}
	return time.Unix(seconds, 0).UTC(), parsedID.String(), true
}

func automaticMaintenanceFingerprint(instanceID string, windowStart time.Time) (string, error) {
	return canonicalRequestFingerprint("automatic-maintenance", instanceID, "update",
		map[string]any{"occurrence": windowStart.UTC().Format(time.RFC3339)})
}

func isAutomaticMaintenanceUpdate(job domain.Job) bool {
	maintenance, _ := job.Payload["maintenance"].(bool)
	window, instanceID, validKey := parseMaintenanceUpdateKey(job.IdempotencyKey)
	return job.Type == "update" && job.RequestedBy == "" && maintenance && validKey && !window.IsZero() && instanceID == job.InstanceID
}

func maintenanceBatchBlocked(activeKeys []string, windowStart time.Time) bool {
	for _, key := range activeKeys {
		activeWindow, _, ok := parseMaintenanceUpdateKey(key)
		if !ok || !activeWindow.Equal(windowStart.UTC()) {
			return true
		}
	}
	return false
}

// AutomaticMaintenanceCandidates returns update-available instances on online
// nodes which are not already owned by an active write operation.
func (s *Store) AutomaticMaintenanceCandidates(ctx context.Context) ([]maintenanceUpdateCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance.id FROM instances instance
		JOIN nodes node ON node.id=instance.node_id
		JOIN instance_update_state update_state ON update_state.instance_id=instance.id
		WHERE node.status='online' AND instance.status IN ('stopped','running','degraded')
		AND update_state.available=true AND COALESCE(json_extract(instance.config,'$.maintenance_cron'),'')<>''
		AND NOT EXISTS (
			SELECT 1 FROM jobs job WHERE job.instance_id=instance.id
			AND `+activeJobSQLPredicate+` AND `+instanceWriteJobSQLPredicate+`
		) AND NOT EXISTS (
			SELECT 1 FROM jobs job WHERE job.instance_id=instance.id AND job.type='check_update'
			AND `+activeJobSQLPredicate+`
		) ORDER BY instance.node_id,instance.id`)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	items := make([]maintenanceUpdateCandidate, 0, len(ids))
	for _, id := range ids {
		instance, err := s.GetInstance(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		items = append(items, maintenanceUpdateCandidate{Instance: instance, Cron: instance.Config.MaintenanceCron})
	}
	return items, nil
}

// QueueAutomaticMaintenanceBatch persists all non-conflicting intents for one
// node and cron occurrence. It never changes instance status; MarkDispatched
// owns that transition. ON CONFLICT DO NOTHING lets a concurrent manual writer
// win one instance without aborting other intents in the node transaction.
func (s *Store) QueueAutomaticMaintenanceBatch(ctx context.Context, nodeID string, windowStart time.Time, candidates []maintenanceUpdateCandidate) (maintenanceBatchResult, error) {
	if _, err := uuid.Parse(nodeID); err != nil || windowStart.IsZero() {
		return maintenanceBatchResult{}, errors.New("invalid maintenance batch identity")
	}
	windowStart = windowStart.UTC().Truncate(time.Second)
	filtered := make([]maintenanceUpdateCandidate, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Instance.NodeID != nodeID || candidate.Instance.ID == "" || strings.TrimSpace(candidate.Cron) == "" {
			continue
		}
		if _, duplicate := seen[candidate.Instance.ID]; duplicate {
			continue
		}
		seen[candidate.Instance.ID] = struct{}{}
		filtered = append(filtered, candidate)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Instance.ID < filtered[j].Instance.ID })

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return maintenanceBatchResult{}, err
	}
	defer tx.Rollback()
	var nodeStatus domain.NodeStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id=?1`, nodeID).Scan(&nodeStatus); errors.Is(err, sql.ErrNoRows) {
		return maintenanceBatchResult{}, ErrNotFound
	} else if err != nil {
		return maintenanceBatchResult{}, err
	} else if nodeStatus != domain.NodeOnline {
		return maintenanceBatchResult{}, nil
	}

	activeRows, err := tx.QueryContext(ctx, `SELECT job.idempotency_key FROM jobs job
		WHERE job.node_id=?1 AND job.type='update' AND job.requested_by IS NULL
		AND json_extract(job.payload,'$.maintenance')=1
		AND job.idempotency_key LIKE 'maintenance:update:%'
		AND `+activeJobSQLPredicate+` ORDER BY job.created_at,job.id`, nodeID)
	if err != nil {
		return maintenanceBatchResult{}, err
	}
	activeKeys := make([]string, 0)
	for activeRows.Next() {
		var key string
		if err := activeRows.Scan(&key); err != nil {
			activeRows.Close()
			return maintenanceBatchResult{}, err
		}
		activeKeys = append(activeKeys, key)
	}
	if err := activeRows.Err(); err != nil {
		activeRows.Close()
		return maintenanceBatchResult{}, err
	}
	activeRows.Close()
	if maintenanceBatchBlocked(activeKeys, windowStart) {
		return maintenanceBatchResult{Blocked: true}, nil
	}

	result := maintenanceBatchResult{Jobs: make([]queuedMaintenanceUpdate, 0, len(filtered))}
	for _, candidate := range filtered {
		key := maintenanceUpdateKey(windowStart, candidate.Instance.ID)
		fingerprint, err := automaticMaintenanceFingerprint(candidate.Instance.ID, windowStart)
		if err != nil {
			return maintenanceBatchResult{}, err
		}
		existing, storedFingerprint, lookupErr := scanJobWithFingerprint(tx.QueryRowContext(ctx, `SELECT `+jobColumnsWithFingerprint+`
			FROM jobs WHERE requested_by IS NULL AND idempotency_key=?1`, key))
		if lookupErr == nil {
			existingWindow, _, validKey := parseMaintenanceUpdateKey(existing.IdempotencyKey)
			if existing.NodeID != nodeID || existing.InstanceID != candidate.Instance.ID || !existingWindow.Equal(windowStart) ||
				!validKey || !isAutomaticMaintenanceUpdate(existing) || !requestFingerprintsEqual(storedFingerprint, fingerprint) {
				return maintenanceBatchResult{}, ErrConflict
			}
			if err := writeAutomaticMaintenanceRecords(ctx, tx, existing, windowStart, candidate.Instance.AvailableBuild); err != nil {
				return maintenanceBatchResult{}, err
			}
			result.Jobs = append(result.Jobs, queuedMaintenanceUpdate{Job: existing, Reused: true})
			continue
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return maintenanceBatchResult{}, lookupErr
		}

		instance, err := scanInstance(tx.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=?1`, candidate.Instance.ID))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return maintenanceBatchResult{}, err
		}
		if instance.NodeID != nodeID || !automaticMaintenanceStatusEligible(instance.Status) || instance.Config.MaintenanceCron != candidate.Cron {
			continue
		}
		var available bool
		var availableBuild string
		if err := tx.QueryRowContext(ctx, `SELECT available,available_build FROM instance_update_state
			WHERE instance_id=?1`, instance.ID).Scan(&available, &availableBuild); errors.Is(err, sql.ErrNoRows) || !available {
			continue
		} else if err != nil {
			return maintenanceBatchResult{}, err
		}
		var hasActiveWriteOrUpdateCheck bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs job WHERE job.instance_id=?1
			AND `+activeJobSQLPredicate+` AND (`+instanceWriteJobSQLPredicate+` OR job.type='check_update'))`, instance.ID).Scan(&hasActiveWriteOrUpdateCheck); err != nil {
			return maintenanceBatchResult{}, err
		}
		if hasActiveWriteOrUpdateCheck {
			continue
		}

		instance.UpdateAvailable, instance.AvailableBuild = available, availableBuild
		payload := automaticMaintenancePayload(instance)
		payloadRaw, err := json.Marshal(payload)
		if err != nil {
			return maintenanceBatchResult{}, err
		}
		job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, InstanceID: instance.ID, Type: "update", Status: domain.JobQueued,
			Payload: payload, IdempotencyKey: key, CreatedAt: time.Now().UTC()}
		insertErr := tx.QueryRowContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,idempotency_key,requested_by,created_at,request_fingerprint)
			VALUES(?1,?2,?3,'update','queued',?4,?5,NULL,?6,?7) ON CONFLICT DO NOTHING RETURNING created_at`,
			job.ID, job.NodeID, job.InstanceID, string(payloadRaw), job.IdempotencyKey, job.CreatedAt, fingerprint).Scan(&job.CreatedAt)
		if errors.Is(insertErr, sql.ErrNoRows) {
			// A manual writer won the per-instance unique index. It affects only
			// this candidate; all other intents remain in the batch transaction.
			continue
		}
		if insertErr != nil {
			return maintenanceBatchResult{}, insertErr
		}
		if err := writeAutomaticMaintenanceRecords(ctx, tx, job, windowStart, availableBuild); err != nil {
			return maintenanceBatchResult{}, err
		}
		result.Jobs = append(result.Jobs, queuedMaintenanceUpdate{Job: job})
	}
	if err := tx.Commit(); err != nil {
		return maintenanceBatchResult{}, err
	}
	return result, nil
}

func automaticMaintenancePayload(instance domain.Instance) map[string]any {
	return map[string]any{"maintenance": true,
		"warning_seconds": append([]int(nil), automaticMaintenanceWarningSeconds...), "instance": instance}
}

func automaticMaintenanceStatusEligible(status domain.InstanceStatus) bool {
	return status == domain.InstanceStopped || status == domain.InstanceRunning || status == domain.InstanceDegraded
}

func writeAutomaticMaintenanceRecords(ctx context.Context, tx *sql.Tx, job domain.Job, windowStart time.Time, availableBuild string) error {
	metadata := map[string]any{"job_id": job.ID, "node_id": job.NodeID, "instance_id": job.InstanceID,
		"window_start": windowStart.UTC().Format(time.RFC3339), "available_build": availableBuild}
	metadataRaw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	auditID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("l4d2panel:maintenance:audit:"+job.ID))
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_name,action,resource_type,resource_id,metadata,created_at)
		VALUES(?1,'maintenance-scheduler','maintenance.update.queued','instance',?2,?3,?4)
		ON CONFLICT(id) DO NOTHING`, auditID, job.InstanceID, string(metadataRaw), job.CreatedAt); err != nil {
		return err
	}
	notificationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("l4d2panel:maintenance:notification:"+job.ID))
	result, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,severity,type,title,message,resource,metadata,created_at)
		VALUES(?1,'info','maintenance.update.queued','Maintenance update queued',
		'A scheduled L4D2 server update was queued for its maintenance window',?2,?3,?4)
		ON CONFLICT(id) DO NOTHING`, notificationID, job.InstanceID, string(metadataRaw), job.CreatedAt)
	if err != nil || resultRowsAffected(result) == 0 {
		return err
	}
	return insertNotificationDeliveriesTx(ctx, tx, notificationID.String(), job.CreatedAt)
}

func (s *Server) runAutomaticMaintenanceUpdates(ctx context.Context, now time.Time) {
	location, err := time.LoadLocation(s.config.Timezone)
	if err != nil {
		s.logger.Error("load automatic maintenance timezone", "timezone", s.config.Timezone, "error", err)
		return
	}
	candidates, err := s.store.AutomaticMaintenanceCandidates(ctx)
	if err != nil {
		s.logger.Error("load automatic maintenance candidates", "error", err)
		return
	}
	type occurrenceGroup struct {
		window     time.Time
		candidates []maintenanceUpdateCandidate
	}
	byNode := make(map[string]map[int64]*occurrenceGroup)
	for _, candidate := range candidates {
		if !s.agents.Connected(candidate.Instance.NodeID) {
			continue
		}
		window, due, err := maintenanceWindowOccurrence(candidate.Cron, location, now)
		if err != nil {
			s.logger.Error("parse persisted maintenance cron", "instance_id", candidate.Instance.ID, "error", err)
			continue
		}
		if !due {
			continue
		}
		windows := byNode[candidate.Instance.NodeID]
		if windows == nil {
			windows = make(map[int64]*occurrenceGroup)
			byNode[candidate.Instance.NodeID] = windows
		}
		group := windows[window.Unix()]
		if group == nil {
			group = &occurrenceGroup{window: window}
			windows[window.Unix()] = group
		}
		group.candidates = append(group.candidates, candidate)
	}
	nodeIDs := make([]string, 0, len(byNode))
	for nodeID := range byNode {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	for _, nodeID := range nodeIDs {
		groups := make([]*occurrenceGroup, 0, len(byNode[nodeID]))
		for _, group := range byNode[nodeID] {
			groups = append(groups, group)
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].window.Before(groups[j].window) })
		for _, group := range groups {
			if !s.agents.Connected(nodeID) {
				break
			}
			batch, err := s.store.QueueAutomaticMaintenanceBatch(ctx, nodeID, group.window, group.candidates)
			if err != nil {
				if !errors.Is(err, ErrConflict) {
					s.logger.Error("queue automatic maintenance batch", "node_id", nodeID, "window", group.window, "error", err)
				}
				break
			}
			if batch.Blocked {
				break
			}
			dispatch := false
			for _, item := range batch.Jobs {
				if item.Job.Status == domain.JobQueued || item.Job.Status == domain.JobDispatched {
					dispatch = true
				}
				if item.Reused {
					continue
				}
				_, _ = s.events.Publish(ctx, Event{Type: "job.queued", ResourceType: "job", ResourceID: item.Job.ID,
					Data: map[string]any{"job_id": item.Job.ID, "node_id": item.Job.NodeID,
						"instance_id": item.Job.InstanceID, "automatic_maintenance": true}}, true)
			}
			if dispatch {
				s.dispatchSoon(nodeID)
			}
		}
	}
}
