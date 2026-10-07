package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// A verification snapshot describes registration, not permission to change
// files. Restoring it only makes existing node assignments visible in Control.
func reconcileMapAssignmentsTx(ctx context.Context, tx *sql.Tx, job domain.Job, mapID string, health *string) error {
	value, present := job.Result["assignment_instance_ids"]
	if !present { // Old Agents and lost node registries cannot supply a snapshot.
		return nil
	}
	items, ok := value.([]any)
	if !ok || len(items) > 10000 || job.CreatedAt.IsZero() {
		return errors.New("Agent returned an invalid map assignment snapshot")
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		id, ok := item.(string)
		if !ok || uuid.Validate(id) != nil || seen[id] {
			return errors.New("Agent returned an invalid map assignment instance ID")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	// A result cannot cause assignments to cross the authenticated node boundary.
	var wrongNode bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM instances WHERE id IN (SELECT value FROM json_each(?1)) AND node_id<>?2)`,
		string(raw), job.NodeID).Scan(&wrongNode); err != nil {
		return err
	}
	if wrongNode {
		return errors.New("Agent returned a cross-node map assignment snapshot")
	}
	// The node lock orders file operations, not delivery of terminal results.
	// Conservatively discard snapshots overlapping any relevant write since
	// Control queued this verification. Include outstanding terminal projections
	// and cancellations; an older task can still change the final registration.
	var concurrent bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs job WHERE job.node_id=?1 AND job.id<>?2
		AND ((job.type IN ('map.assign','map.unassign') AND json_extract(job.payload,'$.map_id')=?3)
			OR job.type IN ('restore','delete','detach')
			OR (job.instance_id IS NOT NULL AND `+instanceWriteJobSQLPredicate+`))
		AND (`+activeJobSQLPredicate+` OR COALESCE(job.finished_at,job.created_at)>=?4))`,
		job.NodeID, job.ID, mapID, job.CreatedAt).Scan(&concurrent); err != nil {
		return err
	}
	if concurrent {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET result=json_set(result,'$.assignment_sync','skipped_concurrent_write'),
			message='地图校验完成；分配关系同步遇到并发任务，请等待任务结束后重新校验' WHERE id=?1`, job.ID)
		return err
	}
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM instances WHERE node_id=?1 AND id IN (SELECT value FROM json_each(?2))`, job.NodeID, string(raw)).Scan(&known); err != nil {
		return err
	}
	missing := len(ids) - known
	if missing > 0 && *health == string(domain.MapHealthy) {
		*health = string(domain.MapDegraded)
	}
	// Never invent instances that were removed from Control. The node retains
	// those registrations and the map-delete hard-link checks still fail closed.
	if _, err := tx.ExecContext(ctx, `DELETE FROM map_assignments WHERE map_id=?1 AND instance_id NOT IN (SELECT value FROM json_each(?2))`, mapID, string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO map_assignments(map_id,instance_id,restart_required,created_at,updated_at)
		SELECT ?1,id,1,?4,?4 FROM instances WHERE node_id=?2 AND id IN (SELECT value FROM json_each(?3))
		ON CONFLICT(map_id,instance_id) DO NOTHING`, mapID, job.NodeID, string(raw), jobProjectionTimestamp(job)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET result=json_set(result,'$.assignment_sync','complete','$.unresolved_assignment_count',?2),
		message=CASE WHEN ?2>0 THEN '地图校验完成；节点仍有已删除或未知实例的分配记录，需要管理员核查' ELSE '地图校验及分配关系同步完成' END WHERE id=?1`, job.ID, missing)
	return err
}
