package control

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleCreateBackupUpload(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	var request struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
	}
	if !decodeJSONWithLimit(w, r, &request, 16<<10) {
		return
	}
	principal := principalFromContext(r.Context())
	upload, reused, err := s.store.CreateBackupUpload(r.Context(), instanceID, request.Name, request.Platform,
		request.Size, request.SHA256, principal.User.ID, r.Header.Get("Idempotency-Key"))
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_backup_upload", err.Error())
		}
		return
	}
	s.audit(r, "instance.backup_upload.created", "instance", instanceID, map[string]any{
		"upload_id": upload.ID, "name": upload.Name, "size": upload.Size, "sha256": upload.SHA256,
	})
	w.Header().Set("Location", backupUploadPath(instanceID, upload.ID))
	status := http.StatusCreated
	if reused {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"upload": upload, "idempotent_replay": reused})
}

func (s *Server) handleGetBackupUpload(w http.ResponseWriter, r *http.Request) {
	upload, err := s.store.GetBackupUpload(r.Context(), chi.URLParam(r, "instanceID"), chi.URLParam(r, "uploadID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upload": upload})
}

func (s *Server) handleCreateBackupUploadChunk(w http.ResponseWriter, r *http.Request) {
	instanceID, uploadID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "uploadID")
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required and must not exceed 200 characters")
		return
	}
	var request struct {
		Offset        int64  `json:"offset"`
		ChunkSHA256   string `json:"chunk_sha256"`
		ContentBase64 string `json:"content_base64"`
	}
	if !decodeJSONWithLimit(w, r, &request, 2<<20) {
		return
	}
	request.ChunkSHA256 = strings.ToLower(strings.TrimSpace(request.ChunkSHA256))
	if request.Offset < 0 || request.ChunkSHA256 == "" || validateSHA256(request.ChunkSHA256) != nil {
		writeError(w, http.StatusBadRequest, "invalid_backup_chunk", "offset or chunk_sha256 is invalid")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(request.ContentBase64)
	if err != nil || len(raw) < 1 || len(raw) > maxHTTPTransferChunk {
		writeError(w, http.StatusBadRequest, "invalid_backup_chunk", "content_base64 must contain between 1 and 1048576 decoded bytes")
		return
	}
	digest := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), request.ChunkSHA256) {
		writeError(w, http.StatusBadRequest, "invalid_backup_chunk", "chunk SHA-256 does not match content_base64")
		return
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "backup-upload-chunk", instanceID+"/"+uploadID,
		"append", request)
	if err != nil {
		s.fail(w, err)
		return
	}
	upload, err := s.store.GetBackupUpload(r.Context(), instanceID, uploadID)
	if err != nil {
		s.fail(w, err)
		return
	}
	payload := map[string]any{
		"upload_id": upload.ID, "offset": request.Offset, "length": len(raw), "chunk_sha256": request.ChunkSHA256,
		"content_base64": request.ContentBase64, "expected_size": upload.Size, "expected_sha256": upload.SHA256,
		"expires_at": upload.ExpiresAt,
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "backup.upload.chunk", payload); err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueBackupUploadChunkWithFingerprint(r.Context(), instanceID, uploadID, payload,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_backup_chunk", err.Error())
		}
		return
	}
	s.audit(r, "instance.backup_upload.chunk_requested", "instance", instanceID, map[string]any{
		"upload_id": uploadID, "job_id": job.ID, "offset": request.Offset, "length": len(raw),
	})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status, "idempotent_replay": reused})
}

// handleGetBackupUploadChunk exposes only the terminal state needed to resume
// an upload. It deliberately does not use the generic job endpoint: a caller
// with backups.restore may upload and recover a backup without also receiving
// jobs.read or jobs.details.
func (s *Server) handleGetBackupUploadChunk(w http.ResponseWriter, r *http.Request) {
	instanceID, uploadID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "uploadID")
	upload, err := s.store.GetBackupUpload(r.Context(), instanceID, uploadID)
	if err != nil {
		s.fail(w, err)
		return
	}
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if job.Type != "backup.upload.chunk" || job.InstanceID != upload.InstanceID || stringFromAny(job.Payload["upload_id"]) != upload.ID {
		s.fail(w, ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": job.ID, "status": job.Status})
}

func (s *Server) handleCompleteBackupUpload(w http.ResponseWriter, r *http.Request) {
	instanceID, uploadID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "uploadID")
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "backup-upload", instanceID, "restore", map[string]any{"upload_id": uploadID})
	if err != nil {
		s.fail(w, err)
		return
	}
	upload, err := s.store.GetBackupUpload(r.Context(), instanceID, uploadID)
	if err != nil {
		s.fail(w, err)
		return
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if upload.RestoreJobID == "" && instance.Status != domain.InstanceStopped && instance.Status != domain.InstanceDegraded {
		writeError(w, http.StatusConflict, "instance_must_be_stopped", "instance must be stopped before restoring an uploaded backup")
		return
	}
	payload := map[string]any{"upload_id": upload.ID, "expected_size": upload.Size, "expected_sha256": upload.SHA256,
		"source_platform": upload.Platform, "instance": instance}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "restore", payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_backup_upload", err.Error())
		return
	}
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueBackupUploadRestoreWithFingerprint(r.Context(), instanceID, uploadID, payload,
		principal.User.ID, r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_backup_upload", err.Error())
		}
		return
	}
	s.audit(r, "instance.backup_upload.restore_requested", "instance", instanceID, map[string]any{
		"upload_id": upload.ID, "job_id": job.ID, "source_platform": upload.Platform,
	})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status, "idempotent_replay": reused})
}

func backupUploadPath(instanceID, uploadID string) string {
	return "/api/v1/instances/" + instanceID + "/backup-uploads/" + uploadID
}
