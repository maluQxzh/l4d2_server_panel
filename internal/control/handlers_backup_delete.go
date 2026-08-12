package control

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	instanceID, backupID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "backupID")
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required and must not exceed 200 characters")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_backup_delete", "backup deletion does not accept a request body")
		return
	}
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "backup", instanceID, "delete", map[string]any{"backup_id": backupID})
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, fingerprint); lookupErr == nil {
		s.dispatchSoon(existing.NodeID)
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": existing.ID, "idempotent_replay": true})
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.fail(w, lookupErr)
		return
	}
	backup, err := s.store.GetBackup(r.Context(), instanceID, backupID)
	if err != nil {
		s.fail(w, err)
		return
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "backup_delete_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "目标 Agent 尚不支持备份删除")
		return
	}
	payload := map[string]any{
		"backup_id":       backup.ID,
		"name":            backup.Name,
		"expected_size":   int64(backup.Size),
		"expected_sha256": strings.ToLower(backup.SHA256),
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "backup.delete", payload,
		principal.User.ID, idempotencyKey, fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.backup.delete_requested", "backup", backupID, map[string]any{"instance_id": instanceID, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID,
		Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}
