package control

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type Event struct {
	ID           int64          `json:"id"`
	Type         string         `json:"type"`
	ResourceType string         `json:"resource_type,omitempty"`
	ResourceID   string         `json:"resource_id,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

func (s *Store) AppendEvent(ctx context.Context, event Event) (Event, error) {
	raw, err := json.Marshal(compactPersistentEventData(event.Data))
	if err != nil {
		return Event{}, err
	}
	if len(raw) > 256<<10 {
		raw, _ = json.Marshal(map[string]any{"truncated": true})
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO event_log(type,resource_type,resource_id,data) VALUES($1,$2,$3,$4)
		RETURNING id,created_at`, event.Type, event.ResourceType, event.ResourceID, raw).Scan(&event.ID, &event.CreatedAt)
	return event, err
}

func compactPersistentEventData(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	result := make(map[string]any, len(data))
	for key, value := range data {
		if key != "job" {
			result[key] = value
			continue
		}
		switch job := value.(type) {
		case domain.Job:
			result[key] = map[string]any{"id": job.ID, "node_id": job.NodeID, "instance_id": job.InstanceID,
				"type": job.Type, "status": job.Status, "progress": job.Progress, "message": job.Message}
		case map[string]any:
			summary := make(map[string]any)
			for _, field := range []string{"id", "node_id", "instance_id", "type", "status", "progress", "message"} {
				if item, ok := job[field]; ok {
					summary[field] = item
				}
			}
			result[key] = summary
		default:
			result[key] = map[string]any{"redacted": true}
		}
	}
	return result
}

func (s *Store) EventsAfter(ctx context.Context, cursor int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		limit = 500
	}
	var rows pgx.Rows
	var err error
	if cursor == 0 {
		// A new client needs the most recent window, not the oldest events ever
		// written. Return that window in ascending order so cursors remain usable.
		rows, err = s.pool.Query(ctx, `SELECT id,type,resource_type,resource_id,data,created_at FROM (
			SELECT id,type,resource_type,resource_id,data,created_at FROM event_log ORDER BY id DESC LIMIT $1
		) recent ORDER BY id`, limit)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT id,type,resource_type,resource_id,data,created_at FROM event_log WHERE id>$1 ORDER BY id LIMIT $2`, cursor, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Event, 0)
	for rows.Next() {
		var event Event
		var raw []byte
		if err := rows.Scan(&event.ID, &event.Type, &event.ResourceType, &event.ResourceID, &raw, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &event.Data); err != nil {
			return nil, err
		}
		items = append(items, event)
	}
	return items, rows.Err()
}

func (s *Store) WriteAudit(ctx context.Context, event domain.AuditEvent) error {
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(event.Metadata)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO audit_events(id,actor_id,actor_name,action,resource_type,resource_id,remote_ip,metadata,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, event.ID, nullableUUID(event.ActorID), event.ActorName, event.Action,
		event.ResourceType, event.ResourceID, event.RemoteIP, raw, event.CreatedAt)
	return err
}

func (s *Store) ListAudit(ctx context.Context, resourceID string, limit int) ([]domain.AuditEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id,COALESCE(actor_id::text,''),actor_name,action,resource_type,resource_id,remote_ip,metadata,created_at
		FROM audit_events WHERE ($1='' OR resource_id=$1) ORDER BY created_at DESC,id DESC LIMIT $2`, resourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.AuditEvent, 0)
	for rows.Next() {
		var event domain.AuditEvent
		var raw []byte
		if err := rows.Scan(&event.ID, &event.ActorID, &event.ActorName, &event.Action, &event.ResourceType,
			&event.ResourceID, &event.RemoteIP, &raw, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &event.Metadata); err != nil {
			return nil, err
		}
		items = append(items, event)
	}
	return items, rows.Err()
}

func (s *Store) AuditByID(ctx context.Context, id string) (domain.AuditEvent, error) {
	var event domain.AuditEvent
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT id,COALESCE(actor_id::text,''),actor_name,action,resource_type,resource_id,remote_ip,metadata,created_at
		FROM audit_events WHERE id=$1`, id).Scan(&event.ID, &event.ActorID, &event.ActorName, &event.Action,
		&event.ResourceType, &event.ResourceID, &event.RemoteIP, &raw, &event.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AuditEvent{}, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(raw, &event.Metadata)
	}
	return event, err
}
