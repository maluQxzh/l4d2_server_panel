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
	"github.com/robfig/cron/v3"
)

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	return s.ListTeamUsers(ctx)
}

func (s *Store) ListPackages(ctx context.Context, instanceID string) ([]domain.AddonPackage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,
		COALESCE(installed_by,''),installed_at FROM addon_packages WHERE instance_id=?1 ORDER BY installed_at DESC,id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.AddonPackage, 0)
	for rows.Next() {
		var item domain.AddonPackage
		if err := rows.Scan(&item.ID, &item.InstanceID, &item.Name, &item.Kind, &item.Version, &item.Source, &item.SourceID,
			&item.SHA256, &item.Path, &item.Enabled, &item.Risk, &item.InstalledBy, &item.InstalledAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) SetPackageEnabled(ctx context.Context, instanceID, packageID string, enabled bool) error {
	tag, err := s.db.ExecContext(ctx, `UPDATE addon_packages SET enabled=?3 WHERE id=?1 AND instance_id=?2`, packageID, instanceID, enabled)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) PackageForToggle(ctx context.Context, instanceID, packageID string) (domain.AddonPackage, string, error) {
	var item domain.AddonPackage
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT id,instance_id,name,kind,version,source,source_id,sha256,enabled,risk,
		COALESCE(installed_by,''),installed_at,path FROM addon_packages WHERE id=?1 AND instance_id=?2`, packageID, instanceID).Scan(
		&item.ID, &item.InstanceID, &item.Name, &item.Kind, &item.Version, &item.Source, &item.SourceID, &item.SHA256,
		&item.Enabled, &item.Risk, &item.InstalledBy, &item.InstalledAt, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AddonPackage{}, "", ErrNotFound
	}
	return item, path, err
}

func (s *Store) ListBackups(ctx context.Context, instanceID string) ([]domain.Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,instance_id,name,kind,path,platform,size,sha256,created_at FROM backups WHERE instance_id=?1 ORDER BY created_at DESC,id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Backup, 0)
	for rows.Next() {
		var item domain.Backup
		var size int64
		if err := rows.Scan(&item.ID, &item.InstanceID, &item.Name, &item.Kind, &item.Path, &item.Platform, &size, &item.SHA256, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Size = uint64(size)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) BackupPath(ctx context.Context, instanceID, backupID string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM backups WHERE id=?1 AND instance_id=?2`, backupID, instanceID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return path, err
}

func (s *Store) GetBackup(ctx context.Context, instanceID, backupID string) (domain.Backup, error) {
	var item domain.Backup
	var size int64
	err := s.db.QueryRowContext(ctx, `SELECT id,instance_id,name,kind,path,platform,size,sha256,created_at
		FROM backups WHERE id=?1 AND instance_id=?2`, backupID, instanceID).Scan(&item.ID, &item.InstanceID, &item.Name,
		&item.Kind, &item.Path, &item.Platform, &size, &item.SHA256, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Backup{}, ErrNotFound
	}
	item.Size = uint64(size)
	return item, err
}

// ConsumeJobSecretResult removes an encrypted transfer body after the HTTP
// response has copied it into memory. The job and its non-sensitive checksum
// metadata remain available for history and audit.
func (s *Store) ConsumeJobSecretResult(ctx context.Context, jobID, jobType, field string) error {
	if field != "content_base64_ciphertext" && field != "entries_ciphertext" {
		return errors.New("unsupported secret result field")
	}
	tag, err := s.db.ExecContext(ctx, `UPDATE jobs SET result=json_remove(result,'$.' || ?3) WHERE id=?1 AND type=?2 AND status='succeeded'
		AND json_type(result,'$.' || ?3) IS NOT NULL`, jobID, jobType, field)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) CreateSchedule(ctx context.Context, instanceID, name, expression, timezone, action string, payload map[string]any, enabled bool) (domain.Schedule, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(action) == "" {
		return domain.Schedule{}, errors.New("schedule name and action are required")
	}
	if _, ok := genericInstanceActionPermission(action); !ok || !scheduledActionAllowed(action) {
		return domain.Schedule{}, errors.New("action is not permitted in a persistent schedule")
	}
	if err := validateScheduledPayload(action, payload); err != nil {
		return domain.Schedule{}, err
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return domain.Schedule{}, errors.New("invalid schedule timezone")
	}
	parsed, err := cron.ParseStandard(expression)
	if err != nil {
		return domain.Schedule{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	next := parsed.Next(time.Now().In(location)).UTC()
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Schedule{}, err
	}
	item := domain.Schedule{ID: uuid.NewString(), InstanceID: instanceID, Name: strings.TrimSpace(name), Cron: expression,
		Timezone: timezone, Action: action, Payload: payload, Enabled: enabled, MisfirePolicy: "skip", NextRunAt: &next, CreatedAt: time.Now().UTC()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO schedules(id,instance_id,name,cron,timezone,action,payload,enabled,misfire_policy,next_run_at,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11)`, item.ID, item.InstanceID, item.Name, item.Cron, item.Timezone,
		item.Action, string(raw), item.Enabled, item.MisfirePolicy, item.NextRunAt, item.CreatedAt)
	return item, err
}

func scanSchedule(row scanner) (domain.Schedule, error) {
	var item domain.Schedule
	var payload []byte
	err := row.Scan(&item.ID, &item.InstanceID, &item.Name, &item.Cron, &item.Timezone, &item.Action, &payload,
		&item.Enabled, &item.MisfirePolicy, &item.LastRunAt, &item.NextRunAt, &item.CreatedAt)
	if err == nil {
		err = json.Unmarshal(payload, &item.Payload)
	}
	return item, err
}

func (s *Store) ListSchedules(ctx context.Context, instanceID string) ([]domain.Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,instance_id,name,cron,timezone,action,payload,enabled,misfire_policy,last_run_at,next_run_at,created_at
		FROM schedules WHERE instance_id=?1 ORDER BY created_at,id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Schedule, 0)
	for rows.Next() {
		item, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) SetScheduleEnabled(ctx context.Context, instanceID, scheduleID string, enabled bool) (domain.Schedule, error) {
	item, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT id,instance_id,name,cron,timezone,action,payload,enabled,misfire_policy,last_run_at,next_run_at,created_at
		FROM schedules WHERE id=?1 AND instance_id=?2`, scheduleID, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Schedule{}, ErrNotFound
	}
	if err != nil {
		return domain.Schedule{}, err
	}
	if enabled {
		location, locationErr := time.LoadLocation(item.Timezone)
		if locationErr != nil {
			return domain.Schedule{}, locationErr
		}
		parsed, parseErr := cron.ParseStandard(item.Cron)
		if parseErr != nil {
			return domain.Schedule{}, parseErr
		}
		next := parsed.Next(time.Now().In(location)).UTC()
		item.NextRunAt = &next
	}
	item.Enabled = enabled
	_, err = s.db.ExecContext(ctx, `UPDATE schedules SET enabled=?3,next_run_at=?4 WHERE id=?1 AND instance_id=?2`, scheduleID, instanceID, enabled, item.NextRunAt)
	return item, err
}

func (s *Store) DeleteSchedule(ctx context.Context, instanceID, scheduleID string) error {
	tag, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id=?1 AND instance_id=?2`, scheduleID, instanceID)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) DueSchedules(ctx context.Context, limit int) ([]domain.Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,instance_id,name,cron,timezone,action,payload,enabled,misfire_policy,last_run_at,next_run_at,created_at
		FROM schedules WHERE enabled AND next_run_at<=?2 ORDER BY next_run_at,id LIMIT ?1`, limit, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Schedule, 0)
	for rows.Next() {
		item, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) AdvanceSchedule(ctx context.Context, schedule domain.Schedule, ranAt time.Time) error {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return err
	}
	parsed, err := cron.ParseStandard(schedule.Cron)
	if err != nil {
		return err
	}
	next := parsed.Next(ranAt.In(location)).UTC()
	_, err = s.db.ExecContext(ctx, `UPDATE schedules SET last_run_at=?2,next_run_at=?3 WHERE id=?1`, schedule.ID, ranAt, next)
	return err
}

func (s *Store) CreateNotification(ctx context.Context, severity, eventType, title, message, resource string, metadata map[string]any) error {
	if severity != "info" && severity != "warning" && severity != "critical" {
		severity = "warning"
	}
	raw, _ := json.Marshal(metadata)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, createdAt := uuid.NewString(), time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,severity,type,title,message,resource,metadata,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8)`, id, severity, eventType, title, message, resource, string(raw), createdAt); err != nil {
		return err
	}
	if err := insertNotificationDeliveriesTx(ctx, tx, id, createdAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateNotificationOnce(ctx context.Context, severity, eventType, title, message, resource string, metadata map[string]any, within time.Duration) error {
	if severity != "info" && severity != "warning" && severity != "critical" {
		severity = "warning"
	}
	if within <= 0 {
		within = time.Hour
	}
	raw, _ := json.Marshal(metadata)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, createdAt := uuid.NewString(), time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,severity,type,title,message,resource,metadata,created_at)
		SELECT ?1,?2,?3,?4,?5,?6,?7,?9 WHERE NOT EXISTS (
			SELECT 1 FROM notifications WHERE type=?3 AND resource=?6 AND created_at>?8)`,
		id, severity, eventType, title, message, resource, string(raw), createdAt.Add(-within), createdAt)
	if err != nil {
		return err
	}
	if resultRowsAffected(result) > 0 {
		if err := insertNotificationDeliveriesTx(ctx, tx, id, createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListNotifications(ctx context.Context, limit int) ([]domain.Notification, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,severity,type,title,message,resource,metadata,read_at,created_at FROM notifications ORDER BY created_at DESC,id DESC LIMIT ?1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Notification, 0)
	for rows.Next() {
		item, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanNotification(row scanner) (domain.Notification, error) {
	var item domain.Notification
	var raw []byte
	err := row.Scan(&item.ID, &item.Severity, &item.Type, &item.Title, &item.Message, &item.Resource, &raw, &item.ReadAt, &item.CreatedAt)
	if err == nil {
		err = json.Unmarshal(raw, &item.Metadata)
	}
	return item, err
}

func (s *Store) Notification(ctx context.Context, id string) (domain.Notification, error) {
	item, err := scanNotification(s.db.QueryRowContext(ctx, `SELECT id,severity,type,title,message,resource,metadata,read_at,created_at
		FROM notifications WHERE id=?1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	return item, err
}

func (s *Store) ReadNotification(ctx context.Context, id string) error {
	tag, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at=COALESCE(read_at,unixepoch('subsec')*1000000) WHERE id=?1`, id)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) LatestSuccessfulJob(ctx context.Context, instanceID, jobType string, payloadFilter map[string]any) (domain.Job, error) {
	filter, _ := json.Marshal(payloadFilter)
	job, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE instance_id=?1 AND type=?2 AND status='succeeded'
		AND NOT EXISTS (SELECT 1 FROM json_each(?3) filter
			WHERE json_extract(jobs.payload,'$.' || filter.key) IS NOT filter.value)
		ORDER BY finished_at DESC,id DESC LIMIT 1`, instanceID, jobType, string(filter)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	return job, err
}

func (s *Store) ApplyJobResult(ctx context.Context, job domain.Job) error {
	if job.Status != domain.JobSucceeded {
		return nil
	}
	if strings.HasPrefix(job.Type, "map.") {
		return s.applyMapJobResult(ctx, job)
	}
	switch job.Type {
	case "environment.inspect":
		dependencies, err := json.Marshal(job.Result["system_dependencies"])
		if err != nil {
			return err
		}
		if string(dependencies) == "null" {
			dependencies = []byte("{}")
		}
		steamError := stringFromAny(job.Result["steamcmd_error"])
		_, err = s.db.ExecContext(ctx, `UPDATE environments SET system_dependencies=?2,
			steamcmd_path=?3,steamcmd_source=?4,steamcmd_status=?5,steamcmd_version=?6,last_error=?7,
			last_inspected_at=?8,updated_at=?8 WHERE node_id=?1`, job.NodeID, string(dependencies),
			stringFromAny(job.Result["steamcmd_path"]), defaultString(stringFromAny(job.Result["steamcmd_source"]), "unconfigured"),
			defaultString(stringFromAny(job.Result["steamcmd_status"]), "unconfigured"), stringFromAny(job.Result["steamcmd_version"]),
			steamError, jobProjectionTimestamp(job))
		if err != nil {
			return err
		}
		if values, ok := job.Result["installations"].([]any); ok {
			for _, value := range values {
				item, ok := value.(map[string]any)
				if !ok || stringFromAny(item["id"]) == "" {
					continue
				}
				status := stringFromAny(item["status"])
				if status != "ready" && status != "degraded" {
					continue
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE game_installations SET status=?3,build_id=COALESCE(NULLIF(?4,''),build_id),
					last_error=?5,last_checked_at=?6,updated_at=?6 WHERE id=?1 AND node_id=?2`, stringFromAny(item["id"]), job.NodeID,
					status, stringFromAny(item["build_id"]), stringFromAny(item["error"]), jobProjectionTimestamp(job)); err != nil {
					return err
				}
				if status == "degraded" {
					if _, err := s.db.ExecContext(ctx, `UPDATE instances SET status='degraded',updated_at=?2 WHERE installation_id=?1
						AND status NOT IN ('starting','running','stopping')`, stringFromAny(item["id"]), jobProjectionTimestamp(job)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	case "steamcmd.configure", "steamcmd.install", "steamcmd.repair":
		_, err := s.db.ExecContext(ctx, `UPDATE environments SET steamcmd_path=?2,steamcmd_source=?3,
			steamcmd_status='ready',steamcmd_version=?4,last_error='',last_inspected_at=?5,updated_at=?5 WHERE node_id=?1`,
			job.NodeID, stringFromAny(job.Result["steamcmd_path"]), stringFromAny(job.Result["steamcmd_source"]),
			stringFromAny(job.Result["steamcmd_version"]), jobProjectionTimestamp(job))
		return err
	case "steamcmd.clear":
		_, err := s.db.ExecContext(ctx, `UPDATE environments SET steamcmd_path='',steamcmd_source='unconfigured',
			steamcmd_status='unconfigured',steamcmd_version='',last_error='',last_inspected_at=?2,updated_at=?2 WHERE node_id=?1`,
			job.NodeID, jobProjectionTimestamp(job))
		return err
	case "game.install", "game.import", "game.validate", "game.update", "game.reinstall":
		_, err := s.db.ExecContext(ctx, `UPDATE game_installations SET status='ready',build_id=COALESCE(NULLIF(?2,''),build_id),
			last_error='',last_checked_at=?3,updated_at=?3 WHERE id=?1`, job.InstallationID,
			stringFromAny(job.Result["build_id"]), jobProjectionTimestamp(job))
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE instances SET build_id=COALESCE(NULLIF(?2,''),build_id),
			status=CASE WHEN status IN ('installing','updating','degraded') THEN 'stopped' ELSE status END,
			updated_at=?3 WHERE installation_id=?1`, job.InstallationID, stringFromAny(job.Result["build_id"]), jobProjectionTimestamp(job))
		return err
	case "game.delete":
		installationID := stringFromAny(job.Payload["installation_id"])
		mode := stringFromAny(job.Payload["mode"])
		filesDeleted, _ := job.Result["files_deleted"].(bool)
		if installationID == "" || stringFromAny(job.Result["installation_id"]) != installationID ||
			stringFromAny(job.Result["path"]) != stringFromAny(job.Payload["path"]) ||
			(mode != "record" && mode != "files") || filesDeleted != (mode == "files") {
			return errors.New("game installation deletion result does not match the request")
		}
		result, err := s.db.ExecContext(ctx, `DELETE FROM game_installations WHERE id=?1 AND node_id=?2
			AND NOT EXISTS(SELECT 1 FROM instances WHERE installation_id=game_installations.id)`, installationID, job.NodeID)
		if err == nil && resultRowsAffected(result) == 0 {
			return ErrNotFound
		}
		return err
	case "start", "restart":
		if stringFromAny(job.Result["status"]) == "stopped" {
			_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='stopped',pid=0,started_at=NULL,updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID)
			return err
		}
		pid := intFromAny(job.Result["pid"])
		startedAt := jobProjectionTimestamp(job)
		if raw, ok := job.Result["started_at"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				startedAt = parsed
			}
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET status='running',pid=?2,started_at=?3,updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID, pid, startedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE map_assignments SET restart_required=0,updated_at=?2 WHERE instance_id=?1`, job.InstanceID, startedAt); err != nil {
			return err
		}
		err = tx.Commit()
		return err
	case "stop":
		_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='stopped',pid=0,started_at=NULL,updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID)
		return err
	case "install", "reinstall":
		path, _ := job.Result["path"].(string)
		buildID := stringFromAny(job.Result["build_id"])
		if restart, ok := job.Result["restart"].(map[string]any); ok {
			pid := intFromAny(restart["pid"])
			startedAt := jobProjectionTimestamp(job)
			if raw := stringFromAny(restart["started_at"]); raw != "" {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
					startedAt = parsed
				}
			}
			_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='running',pid=?2,started_at=?3,
				path=COALESCE(NULLIF(?4,''),path),build_id=COALESCE(NULLIF(?5,''),build_id),updated_at=unixepoch('subsec')*1000000 WHERE id=?1`,
				job.InstanceID, pid, startedAt, path, buildID)
			return err
		}
		_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='stopped',path=COALESCE(NULLIF(?2,''),path),
			build_id=COALESCE(NULLIF(?3,''),build_id),updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID, path, buildID)
		return err
	case "import":
		path, _ := job.Result["path"].(string)
		buildID := stringFromAny(job.Result["build_id"])
		status := "stopped"
		pid := 0
		if likelyRunning, _ := job.Result["likely_running"].(bool); likelyRunning {
			status, pid = "external", intFromAny(job.Result["pid"])
		}
		_, err := s.db.ExecContext(ctx, `UPDATE instances SET status=?2,pid=?3,path=COALESCE(NULLIF(?4,''),path),
			build_id=COALESCE(NULLIF(?5,''),build_id),updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID, status, pid, path, buildID)
		if err == nil && job.InstallationID != "" {
			_, err = s.db.ExecContext(ctx, `UPDATE game_installations SET status='ready',build_id=COALESCE(NULLIF(?2,''),build_id),
				last_error='',last_checked_at=?3,updated_at=?3 WHERE id=?1`, job.InstallationID, buildID, jobProjectionTimestamp(job))
		}
		return err
	case "update", "validate":
		buildID := stringFromAny(job.Result["build_id"])
		if restart, ok := job.Result["restart"].(map[string]any); ok {
			pid := intFromAny(restart["pid"])
			startedAt := jobProjectionTimestamp(job)
			if raw := stringFromAny(restart["started_at"]); raw != "" {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
					startedAt = parsed
				}
			}
			_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='running',pid=?2,started_at=?3,
				build_id=COALESCE(NULLIF(?4,''),build_id),updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID, pid, startedAt, buildID)
			if err != nil || job.Type != "update" {
				return err
			}
			return s.clearAvailableUpdateAfterInstall(ctx, job.InstanceID, buildID)
		}
		_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='stopped',pid=0,started_at=NULL,
			build_id=COALESCE(NULLIF(?2,''),build_id),updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID, buildID)
		if err != nil || job.Type != "update" {
			return err
		}
		return s.clearAvailableUpdateAfterInstall(ctx, job.InstanceID, buildID)
	case "a2s.info":
		info, _ := job.Result["info"].(map[string]any)
		_, err := s.db.ExecContext(ctx, `UPDATE instances SET map=?2,players=?3,max_players=?4,build_id=?5,updated_at=unixepoch('subsec')*1000000 WHERE id=?1`,
			job.InstanceID, stringFromAny(info["map"]), intFromAny(info["players"]), intFromAny(info["max_players"]), stringFromAny(info["version"]))
		return err
	case "backup.upload.chunk":
		return s.applyBackupUploadChunkResult(ctx, job)
	case "backup.upload.abort":
		if stringFromAny(job.Result["upload_id"]) != stringFromAny(job.Payload["upload_id"]) || stringFromAny(job.Result["upload_id"]) == "" {
			return errors.New("Agent returned invalid backup upload abandonment metadata")
		}
		return nil
	case "restore":
		// Restore completion and all of its projections commit atomically in
		// UpdateJob. Re-applying them here would reopen a post-commit race with a
		// newly queued configuration write.
		return nil
	case "backup":
		name := stringFromAny(job.Result["name"])
		path := stringFromAny(job.Result["path"])
		if name == "" || path == "" {
			return nil
		}
		createdAt := time.Now().UTC()
		if raw := stringFromAny(job.Result["created_at"]); raw != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				createdAt = parsed
			}
		}
		kind := stringFromAny(job.Result["kind"])
		if kind == "" {
			kind = stringFromAny(job.Payload["kind"])
		}
		if kind == "" {
			kind = "manual"
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `INSERT INTO backups(id,instance_id,name,kind,path,platform,size,sha256,created_at)
			VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9) ON CONFLICT(id) DO NOTHING`, job.ID, job.InstanceID, name, kind, path,
			stringFromAny(job.Result["platform"]), intFromAny(job.Result["size"]), stringFromAny(job.Result["sha256"]), createdAt); err != nil {
			return err
		}
		if weekly, ok := job.Result["weekly"].(map[string]any); ok {
			weeklyPath := stringFromAny(weekly["path"])
			weeklyName := stringFromAny(weekly["name"])
			if weeklyPath != "" && weeklyName != "" {
				weeklyID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(job.ID+":weekly"))
				if _, err = tx.ExecContext(ctx, `INSERT INTO backups(id,instance_id,name,kind,path,platform,size,sha256,created_at)
					VALUES(?1,?2,?3,'weekly',?4,?5,?6,?7,?8) ON CONFLICT(id) DO NOTHING`, weeklyID, job.InstanceID, weeklyName,
					weeklyPath, stringFromAny(weekly["platform"]), intFromAny(weekly["size"]), stringFromAny(weekly["sha256"]), createdAt); err != nil {
					return err
				}
			}
		}
		prunedPaths := make([]string, 0)
		if values, ok := job.Result["pruned_paths"].([]any); ok {
			for _, value := range values {
				if candidate := stringFromAny(value); candidate != "" {
					prunedPaths = append(prunedPaths, candidate)
				}
			}
		}
		if len(prunedPaths) > 0 {
			rawPaths, marshalErr := json.Marshal(prunedPaths)
			if marshalErr != nil {
				return marshalErr
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM backups WHERE instance_id=?1
				AND path IN (SELECT value FROM json_each(?2))`, job.InstanceID, string(rawPaths)); err != nil {
				return err
			}
		}
		return tx.Commit()
	case "install_mod":
		return s.applyInstallModResult(ctx, job)
	case "qq_flash_import":
		if stringFromAny(job.Payload["mode"]) == "managed_vpk_v2" {
			return s.applyQQFlashPackageResult(ctx, job)
		}
		return nil
	case "rollback_package":
		return s.applyPackageRollbackResult(ctx, job)
	case "toggle_package":
		packageID := stringFromAny(job.Payload["package_id"])
		enabled, _ := job.Payload["enabled"].(bool)
		path := stringFromAny(job.Result["path"])
		_, err := s.db.ExecContext(ctx, `UPDATE addon_packages SET enabled=?3,path=COALESCE(NULLIF(?4,''),path)
			WHERE id=?1 AND instance_id=?2`, packageID, job.InstanceID, enabled, path)
		return err
	case "delete_package":
		packageID := stringFromAny(job.Payload["package_id"])
		if packageID == "" || stringFromAny(job.Result["path"]) != stringFromAny(job.Payload["package_path"]) ||
			!strings.EqualFold(stringFromAny(job.Result["sha256"]), stringFromAny(job.Payload["expected_sha256"])) {
			return errors.New("package deletion result does not match the requested package")
		}
		result, err := s.db.ExecContext(ctx, `DELETE FROM addon_packages WHERE id=?1 AND instance_id=?2`, packageID, job.InstanceID)
		if err == nil && resultRowsAffected(result) == 0 {
			return ErrNotFound
		}
		return err
	case "ban_player":
		steamID := stringFromAny(job.Payload["steam_id"])
		if steamID == "" {
			return nil
		}
		reason := strings.TrimSpace(stringFromAny(job.Payload["reason"]))
		if reason == "" {
			reason = "Banned by an administrator"
		}
		projectedAt := jobProjectionTimestamp(job)
		var expiresAt any
		if duration := intFromAny(job.Payload["duration_minutes"]); duration > 0 {
			expiresAt = projectedAt.Add(time.Duration(duration) * time.Minute)
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO player_bans(id,instance_id,steam_id,reason,created_by,expires_at,created_at)
			VALUES(?1,?2,?3,?4,?5,?6,?7) ON CONFLICT(instance_id,steam_id) DO UPDATE SET
			reason=EXCLUDED.reason,created_by=EXCLUDED.created_by,expires_at=EXCLUDED.expires_at,created_at=EXCLUDED.created_at`,
			uuid.NewString(), job.InstanceID, steamID, reason, nullableUUID(job.RequestedBy), expiresAt, projectedAt)
		return err
	case "unban_player":
		_, err := s.db.ExecContext(ctx, `DELETE FROM player_bans WHERE instance_id=?1 AND steam_id=?2`, job.InstanceID, stringFromAny(job.Payload["steam_id"]))
		return err
	case "delete", "detach":
		err := s.ApplyInstanceRemovalResult(ctx, job)
		if errors.Is(err, ErrNotFound) {
			// Replayed terminal results arrive after the original delete has
			// already removed the instance and nulled jobs.instance_id.
			return nil
		}
		return err
	case "purge_quarantine":
		return s.ApplyQuarantinePurgeResult(ctx, job)
	}
	return nil
}

func (s *Store) ApplyJobFailure(ctx context.Context, job domain.Job) error {
	if job.Status != domain.JobFailed {
		return nil
	}
	if job.Type == "start" || job.Type == "stop" || job.Type == "restart" || job.Type == "install" ||
		job.Type == "reinstall" || job.Type == "import" || job.Type == "configure" || job.Type == "update" || job.Type == "validate" {
		_, err := s.db.ExecContext(ctx, `UPDATE instances SET status='degraded',updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID)
		if err == nil && job.InstallationID != "" && (job.Type == "import" || job.Type == "install" || job.Type == "reinstall" || job.Type == "update" || job.Type == "validate") {
			_, err = s.db.ExecContext(ctx, `UPDATE game_installations SET status='degraded',last_error=?2,last_checked_at=?3,updated_at=?3 WHERE id=?1`,
				job.InstallationID, job.Message, jobProjectionTimestamp(job))
		}
		return err
	}
	if strings.HasPrefix(job.Type, "game.") && job.InstallationID != "" {
		_, err := s.db.ExecContext(ctx, `UPDATE game_installations SET status='degraded',last_error=?2,
			last_checked_at=?3,updated_at=?3 WHERE id=?1`, job.InstallationID, job.Message, jobProjectionTimestamp(job))
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE instances SET status='degraded',updated_at=?2 WHERE installation_id=?1`,
			job.InstallationID, jobProjectionTimestamp(job))
		return err
	}
	if strings.HasPrefix(job.Type, "steamcmd.") || job.Type == "environment.inspect" {
		// SteamCMD changes are transactional on the Agent: a failed configure,
		// install, repair, or clear leaves the previously active launcher intact.
		// Preserve Control's last known-good source/path/status as well.
		_, err := s.db.ExecContext(ctx, `UPDATE environments SET last_error=?2,updated_at=?3 WHERE node_id=?1`,
			job.NodeID, job.Message, jobProjectionTimestamp(job))
		return err
	}
	if job.Type == "purge_quarantine" {
		return s.ReleaseQuarantinePurgeJob(ctx, job)
	}
	if job.Type == "backup.upload.chunk" {
		return s.scrubBackupUploadChunkBody(ctx, job)
	}
	if job.Type == "restore" {
		// UpdateJob atomically published the failed upload state.
		return nil
	}
	if strings.HasPrefix(job.Type, "map.") {
		mapID := stringFromAny(job.Payload["map_id"])
		if job.Type == "map.delete" {
			_, err := s.db.ExecContext(ctx, `UPDATE maps SET health='degraded',updated_at=?2 WHERE id=?1`, mapID, jobProjectionTimestamp(job))
			return err
		}
		if job.Type == "map.verify" || job.Type == "map.inventory" {
			_, err := s.db.ExecContext(ctx, `UPDATE maps SET health=CASE WHEN health='healthy' THEN 'degraded' ELSE health END,updated_at=?2 WHERE id=?1`, mapID, jobProjectionTimestamp(job))
			return err
		}
		return nil
	}
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func jobProjectionTimestamp(job domain.Job) time.Time {
	if job.FinishedAt != nil && !job.FinishedAt.IsZero() {
		return job.FinishedAt.UTC()
	}
	if !job.CreatedAt.IsZero() {
		return job.CreatedAt.UTC()
	}
	return time.Unix(0, 0).UTC()
}

// applyJobCancellationTx publishes every cancellation side effect in the same
// commit that settles the job. This is deliberately not exposed as a
// post-commit replay API: once the active-job index is released, a stale
// cancellation projection must never overwrite the state of newer work.
func applyJobCancellationTx(ctx context.Context, tx *sql.Tx, job domain.Job) error {
	if job.Status != domain.JobCanceled {
		return nil
	}
	if strings.HasPrefix(job.Type, "map.") {
		mapID := strings.TrimSpace(stringFromAny(job.Payload["map_id"]))
		if mapID == "" {
			return nil
		}
		switch job.Type {
		case "map.delete":
			_, err := tx.ExecContext(ctx, `UPDATE maps SET health='degraded',updated_at=?2 WHERE id=?1 AND health='deleting'`, mapID, jobProjectionTimestamp(job))
			return err
		case "map.verify", "map.inventory":
			_, err := tx.ExecContext(ctx, `UPDATE maps SET health=CASE WHEN health='healthy' THEN 'degraded' ELSE health END,updated_at=?2 WHERE id=?1`, mapID, jobProjectionTimestamp(job))
			return err
		default:
			return nil
		}
	}
	if job.Type == "restore" {
		if job.InstanceID == "" {
			return nil
		}
		return applyRestoreTerminalProjectionTx(ctx, tx, job)
	}
	if job.Type == "purge_quarantine" {
		return releaseQuarantinePurgeJobTx(ctx, tx, job)
	}
	if job.Type == "backup.upload.chunk" {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET payload=json_remove(payload,'$.content_base64_ciphertext') WHERE id=?1`, job.ID)
		return err
	}
	if isAutomaticMaintenanceUpdate(job) {
		// Automatic intents leave the instance in its real state while queued
		// and persist that state in the canonical lifecycle snapshot. Once a
		// dispatched countdown/update is canceled, restore the snapshot instead
		// of publishing the generic degraded cancellation state.
		snapshot, ok := job.Payload["instance"].(map[string]any)
		status := domain.InstanceStatus(stringFromAny(snapshot["status"]))
		if !ok || (status != domain.InstanceStopped && status != domain.InstanceRunning && status != domain.InstanceDegraded) {
			return errors.New("automatic maintenance job is missing its instance status snapshot")
		}
		_, err := tx.ExecContext(ctx, `UPDATE instances SET status=?2,updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID, status)
		return err
	}
	switch job.Type {
	case "game.install", "game.import", "game.validate", "game.update", "game.reinstall":
		if job.InstallationID == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE game_installations SET status='degraded',last_error='operation canceled',updated_at=?2 WHERE id=?1`,
			job.InstallationID, jobProjectionTimestamp(job))
		return err
	case "start", "stop", "restart", "install", "reinstall", "import", "configure", "update", "validate":
		if job.InstanceID == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET status='degraded',updated_at=unixepoch('subsec')*1000000 WHERE id=?1`, job.InstanceID); err != nil {
			return err
		}
	default:
	}
	return nil
}

func intFromAny(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		v, _ := n.Int64()
		return int(v)
	default:
		return 0
	}
}

func stringFromAny(value any) string {
	valueString, _ := value.(string)
	return valueString
}

func riskFromPayload(payload map[string]any) string {
	if confirmed, _ := payload["risk_confirmed"].(bool); confirmed {
		return "confirmed_native"
	}
	return "normal"
}
