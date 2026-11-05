package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// Materialize authorized resource IDs before the history LIMIT. Unknown or
// deleted resource ownership is not inferred from an untrusted event type.
func historyScopeSQL(scope *jobVisibilityScope) (string, []any) {
	if scope == nil {
		return "", nil
	}
	nodes, _ := json.Marshal(scope.NodeIDs)
	instances, _ := json.Marshal(scope.InstanceIDs)
	legacy, _ := json.Marshal(scope.LegacyOwnNodeIDs)
	return `WITH allowed_nodes AS (SELECT value AS id FROM json_each(?1)),
	visible_instances AS (SELECT id FROM instances WHERE node_id IN (SELECT id FROM allowed_nodes) OR id IN (SELECT value FROM json_each(?2))),
	visible_jobs AS (SELECT id FROM jobs WHERE node_id IN (SELECT id FROM allowed_nodes) OR instance_id IN (SELECT id FROM visible_instances)
	 OR (instance_id IS NULL AND type IN ('map.import.qq_flash','map.verify','map.delete') AND (
	 json_extract(payload,'$._scope_instance_id') IN (SELECT id FROM visible_instances)
	 OR (COALESCE(json_extract(payload,'$._scope_instance_id'),'')='' AND requested_by=?3 AND node_id IN (SELECT value FROM json_each(?4)))))),
	visible_resources AS (
	 SELECT id FROM nodes WHERE id IN (SELECT id FROM allowed_nodes)
	 UNION SELECT id FROM visible_instances UNION SELECT id FROM visible_jobs
	 UNION SELECT id FROM schedules WHERE instance_id IN (SELECT id FROM visible_instances)
	 UNION SELECT id FROM addon_packages WHERE instance_id IN (SELECT id FROM visible_instances)
	 UNION SELECT id FROM backups WHERE instance_id IN (SELECT id FROM visible_instances)) `, []any{string(nodes), string(instances), scope.RequestedBy, string(legacy)}
}

func historyWindow(where string, args []any, cursor *jobListCursor, limit int) (string, []any) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	if cursor != nil {
		args = append(args, cursor.CreatedAt, cursor.ID)
		where += fmt.Sprintf(` AND (created_at,id)<(?%d,?%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	return where + fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT ?%d`, len(args)), args
}

func historyNext(at time.Time, id string) string {
	raw, _ := json.Marshal(jobListCursor{CreatedAt: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Store) ListVisibleAuditPage(ctx context.Context, resourceID string, scope *jobVisibilityScope, cursor *jobListCursor, limit int) ([]domain.AuditEvent, string, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	cte, args := historyScopeSQL(scope)
	args = append(args, resourceID)
	where := fmt.Sprintf(` WHERE (?%d='' OR resource_id=?%d)`, len(args), len(args))
	if scope != nil {
		where += ` AND CASE
	 WHEN resource_type='node' THEN resource_id IN (SELECT id FROM allowed_nodes)
	 WHEN resource_type='instance' AND EXISTS(SELECT 1 FROM instances WHERE id=resource_id) THEN resource_id IN (SELECT id FROM visible_instances)
	 WHEN resource_type='job' AND EXISTS(SELECT 1 FROM jobs WHERE id=resource_id) THEN resource_id IN (SELECT id FROM visible_jobs)
	 WHEN EXISTS(SELECT 1 FROM instances WHERE id=json_extract(metadata,'$.instance_id')) THEN json_extract(metadata,'$.instance_id') IN (SELECT id FROM visible_instances)
	 ELSE resource_id IN (SELECT id FROM visible_resources) END`
	}
	window, args := historyWindow(where, args, cursor, limit)
	rows, err := s.db.QueryContext(ctx, cte+`SELECT id,COALESCE(actor_id,''),actor_name,action,resource_type,resource_id,remote_ip,metadata,created_at FROM audit_events`+window, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]domain.AuditEvent, 0)
	for rows.Next() {
		var item domain.AuditEvent
		var raw []byte
		if err := rows.Scan(&item.ID, &item.ActorID, &item.ActorName, &item.Action, &item.ResourceType, &item.ResourceID, &item.RemoteIP, &raw, &item.CreatedAt); err != nil {
			return nil, "", err
		}
		if err := json.Unmarshal(raw, &item.Metadata); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = historyNext(last.CreatedAt, last.ID)
	}
	return items, next, nil
}

func (s *Store) ListVisibleNotificationPage(ctx context.Context, scope *jobVisibilityScope, cursor *jobListCursor, limit int) ([]domain.Notification, string, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	cte, args := historyScopeSQL(scope)
	where := ` WHERE 1`
	if scope != nil {
		where += ` AND resource<>'' AND CASE
	 WHEN type GLOB 'node.*' THEN resource IN (SELECT id FROM allowed_nodes)
	 WHEN type GLOB 'instance.*' OR type GLOB 'monitoring.*' OR type GLOB 'update.*' THEN resource IN (SELECT id FROM visible_instances)
	 WHEN type GLOB 'job.*' THEN resource IN (SELECT id FROM visible_jobs) ELSE 0 END`
	}
	window, args := historyWindow(where, args, cursor, limit)
	rows, err := s.db.QueryContext(ctx, cte+`SELECT id,severity,type,title,message,resource,metadata,read_at,created_at FROM notifications`+window, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]domain.Notification, 0)
	for rows.Next() {
		item, err := scanNotification(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = historyNext(last.CreatedAt, last.ID)
	}
	return items, next, nil
}
