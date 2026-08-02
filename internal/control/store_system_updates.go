package control

import (
	"context"

	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
)

// SyncSystemUpdate mirrors the ACL-protected filesystem journal into SQLite.
// The journal remains authoritative while the service is stopped; startup
// reconciliation advances this row and then emits the durable audit/event.
func (s *Store) SyncSystemUpdate(ctx context.Context, state portableupdate.State, requestedBy string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO system_update_jobs(
		id,requested_by,status,phase,current_version,target_version,package_sha256,upload_bytes,rollback,error,created_at,updated_at,completed_at
	) VALUES(?1,NULLIF(?2,''),?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13)
	ON CONFLICT(id) DO UPDATE SET
		requested_by=COALESCE(system_update_jobs.requested_by,excluded.requested_by),
		status=excluded.status,phase=excluded.phase,current_version=excluded.current_version,
		target_version=excluded.target_version,package_sha256=excluded.package_sha256,
		upload_bytes=excluded.upload_bytes,rollback=excluded.rollback,error=excluded.error,
		updated_at=excluded.updated_at,completed_at=excluded.completed_at`,
		state.JobID, requestedBy, state.Status, state.Phase, state.CurrentVersion, state.TargetVersion,
		state.PackageSHA256, state.UploadBytes, state.Rollback, state.Error, state.CreatedAt, state.UpdatedAt, state.CompletedAt)
	return err
}
