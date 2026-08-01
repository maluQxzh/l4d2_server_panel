package control

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleAbortBackupUpload(w http.ResponseWriter, r *http.Request) {
	instanceID, uploadID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "uploadID")
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required and must not exceed 200 characters")
		return
	}
	// This endpoint has no request document. Rejecting even a one-byte body
	// keeps request equivalence explicit and avoids accidentally persisting
	// caller-supplied content in a future job payload.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_backup_upload_abort", "request body could not be read")
		return
	}
	if len(body) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_backup_upload_abort", "backup upload abandonment does not accept a request body")
		return
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "backup-upload-abort", instanceID+"/"+uploadID,
		"abort", map[string]any{})
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueBackupUploadAbortWithFingerprint(r.Context(), instanceID, uploadID,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.backup_upload.abort_requested", "instance", instanceID, map[string]any{
		"upload_id": uploadID, "job_id": job.ID, "idempotent_replay": reused,
	})
	if !reused {
		_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID,
			Data: map[string]any{"job": job}}, true)
		s.dispatchSoon(job.NodeID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status, "idempotent_replay": reused})
}
