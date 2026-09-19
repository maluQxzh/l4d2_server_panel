package control

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// This snapshot belongs only to Control, never to the Agent payload/result.
// Returning an already committed outcome makes projection replay a no-op even
// when the imported package has subsequently been deleted or renamed.
func packageScanProjectionTx(ctx context.Context, tx *sql.Tx, job domain.Job) (bool, error) {
	var outcome string
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT r.outcome,r.eligible=1 AND r.generation=i.package_scan_generation
		AND NOT EXISTS(SELECT 1 FROM jobs w WHERE w.instance_id=i.id AND w.holds_instance_write_lock=1)
		FROM package_scan_runs r JOIN instances i ON i.id=r.instance_id JOIN jobs j ON j.id=r.job_id
		WHERE r.job_id=?1 AND r.instance_id=?2 AND i.node_id=?3 AND j.type='scan_packages'`, job.ID, job.InstanceID, job.NodeID).Scan(&outcome, &current)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent delete/detach can remove the instance before the scan
		// returns. Its FK clears jobs.instance_id; there is no inventory left
		// to adopt. Acknowledge that terminal result without recreating scope.
		var deleted bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE id=?1 AND node_id=?2 AND type='scan_packages' AND instance_id IS NULL)`, job.ID, job.NodeID).Scan(&deleted); err != nil {
			return false, err
		}
		if deleted {
			return false, nil
		}
		return false, errors.New("package scan has no matching Control snapshot")
	}
	if err != nil {
		return false, err
	}
	if outcome != "pending" {
		return false, nil
	}
	if !current {
		_, err = tx.ExecContext(ctx, `UPDATE package_scan_runs SET outcome='stale' WHERE job_id=?1`, job.ID)
		return false, err
	}
	return true, nil
}

func (s *Server) handleInstalledPackageScanStatus(w http.ResponseWriter, r *http.Request) {
	// This view needs packages.install, not access to arbitrary job payloads.
	var id, status, outcome string
	var progress, registered int
	var busy bool
	err := s.store.db.QueryRowContext(r.Context(), `SELECT j.id,j.status,j.progress,j.holds_instance_scan_slot,COALESCE(r.outcome,'stale'),COALESCE(r.registered_count,0)
		FROM jobs j LEFT JOIN package_scan_runs r ON r.job_id=j.id WHERE j.instance_id=?1 AND j.type='scan_packages'
		ORDER BY j.created_at DESC,j.id DESC LIMIT 1`, chi.URLParam(r, "instanceID")).Scan(&id, &status, &progress, &busy, &outcome, &registered)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"scan": nil})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scan": map[string]any{"job_id": id, "status": status, "progress": progress, "busy": busy, "outcome": outcome, "registered_count": registered}})
}
