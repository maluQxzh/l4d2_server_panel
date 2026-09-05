package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleRenamePackage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name     string `json:"name"`
		Filename string `json:"filename"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	instanceID, packageID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "packageID")
	payload := map[string]any{"package_id": packageID, "name": strings.TrimSpace(request.Name), "filename": request.Filename}
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "package", instanceID, "rename_package", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principalFromContext(r.Context()).User.ID, key, fingerprint)
		if lookupErr == nil {
			s.dispatchSoon(existing.NodeID)
			writeJSON(w, http.StatusAccepted, map[string]any{"job_id": existing.ID, "idempotent_replay": true})
			return
		}
		if !errors.Is(lookupErr, ErrNotFound) {
			s.fail(w, lookupErr)
			return
		}
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "package_rename_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "请先升级目标 Agent，以支持包重命名")
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "rename_package", payload, principalFromContext(r.Context()).User.ID, r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_package_rename", err.Error())
		}
		return
	}
	s.audit(r, "package.rename.requested", "package", packageID, map[string]any{"instance_id": instanceID, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

// Build from current records while holding the immediate transaction and the
// instance write barrier. Callers cannot supply source paths or snapshot lists.
func packageRenamePayload(ctx context.Context, tx *sql.Tx, instanceID string, status domain.InstanceStatus, input map[string]any) (map[string]any, error) {
	if err := instanceWriteConflict(ctx, tx, instanceID); err != nil {
		return nil, err
	}
	name, filename, id := stringFromAny(input["name"]), stringFromAny(input["filename"]), stringFromAny(input["package_id"])
	if err := domain.ValidatePackageName(name); err != nil {
		return nil, err
	}
	var kind, source, sha string
	if err := tx.QueryRowContext(ctx, `SELECT kind,path,sha256 FROM addon_packages WHERE instance_id=?1 AND id=?2`, instanceID, id).Scan(&kind, &source, &sha); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	output := map[string]any{"package_id": id, "name": name, "kind": kind, "path": source, "target_path": source, "expected_sha256": sha}
	if filename == "" {
		return output, nil
	}
	target, oldCanonical, newCanonical, err := domain.PackageRenamePaths(kind, source, filename)
	if err != nil {
		return nil, err
	}
	if target == source {
		return output, nil
	}
	if status != domain.InstanceStopped {
		return nil, fmt.Errorf("请先停止实例，再修改包文件名: %w", ErrConflict)
	}
	if !validPackageSHA(sha) {
		return nil, errors.New("该包缺少已验证的 SHA-256，请先重新扫描或安装")
	}
	var conflict bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM addon_packages WHERE instance_id=?1 AND id<>?2 AND (lower(path)=lower(?3) OR lower(path)=lower(?4) OR lower(replace(replace(path,'/panel-disabled/','/'),'/plugins/disabled/','/plugins/'))=lower(?4)) UNION ALL SELECT 1 FROM addon_package_files WHERE instance_id=?1 AND package_id<>?2 AND (lower(path)=lower(?3) OR lower(path)=lower(?4)))`, instanceID, id, target, newCanonical).Scan(&conflict); err != nil {
		return nil, err
	}
	if conflict {
		return nil, fmt.Errorf("目标名称已被其他包占用: %w", ErrConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT storage_path,sha256 FROM addon_package_versions WHERE instance_id=?1 AND package_id=?2 ORDER BY id`, instanceID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	snapshots := []any{}
	for rows.Next() {
		var storage, hash string
		if err := rows.Scan(&storage, &hash); err != nil {
			return nil, err
		}
		if _, err := domain.RenamedPackageSnapshot(instanceID, oldCanonical, newCanonical, storage); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, map[string]any{"storage_path": storage, "sha256": hash})
	}
	if len(snapshots) > 3 {
		return nil, errors.New("包版本历史超过限制")
	}
	output["target_path"], output["snapshots"] = target, snapshots
	return output, rows.Err()
}

func (s *Store) applyPackageRenameResult(ctx context.Context, job domain.Job) error {
	id, name := stringFromAny(job.Payload["package_id"]), stringFromAny(job.Payload["name"])
	source, target := stringFromAny(job.Payload["path"]), stringFromAny(job.Payload["target_path"])
	if domain.ValidatePackageName(name) != nil || id == "" || stringFromAny(job.Result["path"]) != target || stringFromAny(job.Result["name"]) != name || stringFromAny(job.Result["sha256"]) != stringFromAny(job.Payload["expected_sha256"]) {
		return errors.New("package rename result does not match request")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE addon_packages SET name=?3,path=?4 WHERE instance_id=?1 AND id=?2 AND path IN (?4,?5)`, job.InstanceID, id, name, target, source)
	if err != nil {
		return err
	}
	if resultRowsAffected(result) != 1 {
		return ErrConflict
	}
	if source != target {
		_, oldCanonical, newCanonical, err := domain.PackageRenamePaths(stringFromAny(job.Payload["kind"]), source, path.Base(target))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,storage_path FROM addon_package_versions WHERE instance_id=?1 AND package_id=?2 AND target_path=?3`, job.InstanceID, id, oldCanonical)
		if err != nil {
			return err
		}
		type change struct{ id, storage string }
		changes := []change{}
		for rows.Next() {
			var c change
			if err := rows.Scan(&c.id, &c.storage); err != nil {
				rows.Close()
				return err
			}
			changes = append(changes, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, c := range changes {
			renamed, err := domain.RenamedPackageSnapshot(job.InstanceID, oldCanonical, newCanonical, c.storage)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE addon_package_versions SET target_path=?2,storage_path=?3,manifest=json_set(manifest,'$.files[0].path',?2) WHERE id=?1`, c.id, newCanonical, renamed); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE addon_package_files SET path=?3 WHERE instance_id=?1 AND package_id=?2 AND path=?4`, job.InstanceID, id, newCanonical, oldCanonical); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// A bounded status view allows package-only operators to run a batch without
// granting general job visibility or exposing payloads and Agent storage paths.
func (s *Server) handlePackageOperationStatus(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if job.InstanceID != chi.URLParam(r, "instanceID") || (job.Type != "toggle_package" && job.Type != "delete_package" && job.Type != "rename_package") {
		s.fail(w, ErrNotFound)
		return
	}
	var busy bool
	if err := s.store.db.QueryRowContext(r.Context(), `SELECT holds_instance_write_lock FROM jobs WHERE id=?1`, job.ID).Scan(&busy); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": job.Status, "busy": busy})
}
