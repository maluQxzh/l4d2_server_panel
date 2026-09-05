package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type instanceBusyError struct {
	jobID  string
	status domain.JobStatus
}

func (e *instanceBusyError) Error() string {
	state, next := "任务进行中", "请在任务中心查看或取消该任务。"
	switch e.status {
	case domain.JobQueued:
		state = "排队中"
	case domain.JobDispatched:
		state = "已派发"
	case domain.JobRunning:
		state = "执行中"
	case domain.JobCanceled:
		state, next = "等待节点确认取消", "请检查节点在线状态及 Agent 日志；收到最终结果后才会释放占用。"
	case domain.JobSucceeded, domain.JobFailed:
		state, next = "任务已结束，结果尚未同步", "中心端会自动重试同步；若持续失败，请按任务 ID 检查中心端日志。"
	}
	return fmt.Sprintf("实例被任务 %s 占用（%s）。%s", e.jobID, state, next)
}

func (e *instanceBusyError) Unwrap() error { return ErrConflict }

func instanceJobWriteConflict(ctx context.Context, tx *sql.Tx, instanceID, jobType string, payload map[string]any) error {
	// Only file.operation participates in lock classification. Do not send a
	// potentially large encrypted upload back through SQLite just to diagnose it.
	var operation any
	if value, ok := payload["operation"].(string); ok {
		operation = value
	}
	var writes bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(`+instanceWriteJobSQLPredicate+`,0)
		FROM (SELECT ?1 AS type,json_object('operation',?2) AS payload) AS job`, jobType, operation).Scan(&writes); err != nil {
		return err
	}
	if !writes {
		return nil // A duplicate read probe is not blocked by the write lock.
	}
	return instanceWriteConflict(ctx, tx, instanceID)
}

// Query only the lock owner and its state, never another task's payload or
// result. Call within the immediate transaction that queues/updates this instance.
func instanceWriteConflict(ctx context.Context, tx *sql.Tx, instanceID string) error {
	var busy instanceBusyError
	err := tx.QueryRowContext(ctx, `SELECT id,status FROM jobs
		WHERE instance_id=?1 AND holds_instance_write_lock=1`, instanceID).Scan(&busy.jobID, &busy.status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return &busy
}
