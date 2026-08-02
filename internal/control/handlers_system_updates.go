package control

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
)

func (s *Server) handleListSystemUpdates(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.updates.Overview())
}

func (s *Server) handleGetSystemUpdate(w http.ResponseWriter, r *http.Request) {
	state, err := s.updates.State(chi.URLParam(r, "jobID"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "system_update_not_found", "center update was not found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleCreateSystemUpdate(w http.ResponseWriter, r *http.Request) {
	if supported, reason := s.updates.Supported(); !supported {
		writeError(w, http.StatusNotImplemented, "system_update_unsupported", reason)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required and must not exceed 200 characters")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, portableupdate.MaxPackageBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "system_update_multipart_required", "multipart package upload is required")
		return
	}
	part, err := nextPackagePart(reader)
	if err != nil {
		writeError(w, http.StatusBadRequest, "system_update_package_required", err.Error())
		return
	}
	defer part.Close()
	principal := principalFromContext(r.Context())
	scopedKey := fmt.Sprintf("%x", sha256.Sum256([]byte(principal.User.ID+"\x00"+idempotencyKey)))
	state, err := s.updates.Create(&singlePackageReader{part: part, multipart: reader}, scopedKey)
	if err != nil {
		systemUpdateError(w, err)
		return
	}
	if err := s.store.SyncSystemUpdate(r.Context(), state, principal.User.ID); err != nil {
		s.logger.Error("sync portable update task", "job_id", state.JobID, "error", err)
	}
	if !state.IdempotentReplay {
		s.audit(r, "system.update.started", "system_update", state.JobID, map[string]any{
			"target_version": state.TargetVersion, "package_sha256": state.PackageSHA256, "actor_id": principal.User.ID,
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": state.JobID, "status": state.Status, "phase": state.Phase, "target_version": state.TargetVersion,
		"idempotent_replay": state.IdempotentReplay,
	})
}

func nextPackagePart(reader *multipart.Reader) (*multipart.Part, error) {
	part, err := reader.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("ZIP package field is required")
	}
	if err != nil {
		return nil, err
	}
	if part.FormName() != "package" {
		part.Close()
		return nil, errors.New("multipart body may contain only the package field")
	}
	if filename := part.FileName(); filename == "" || !strings.EqualFold(filepath.Ext(filename), ".zip") {
		part.Close()
		return nil, errors.New("package must be a ZIP file")
	}
	return part, nil
}

type singlePackageReader struct {
	part      io.Reader
	multipart *multipart.Reader
	checked   bool
}

func (r *singlePackageReader) Read(buffer []byte) (int, error) {
	count, err := r.part.Read(buffer)
	if !errors.Is(err, io.EOF) || r.checked {
		return count, err
	}
	r.checked = true
	extra, nextErr := r.multipart.NextPart()
	if errors.Is(nextErr, io.EOF) {
		return count, io.EOF
	}
	if nextErr != nil {
		return count, nextErr
	}
	_ = extra.Close()
	return count, errors.New("multipart body contains more than one part")
}

func systemUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, portableupdate.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "system_update_unsupported", err.Error())
	case errors.Is(err, portableupdate.ErrUpdateActive):
		writeError(w, http.StatusConflict, "system_update_active", err.Error())
	case errors.Is(err, portableupdate.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "system_update_invalid_package", "update package validation failed")
	}
}

func (s *Server) reconcilePortableUpdates(ctx context.Context) {
	for _, state := range s.updates.PendingReconciliation() {
		if err := s.store.SyncSystemUpdate(ctx, state, ""); err != nil {
			s.logger.Error("reconcile portable update task", "job_id", state.JobID, "error", err)
			continue
		}
		action := "system.update." + state.Status
		event := domain.AuditEvent{ActorName: "portable-updater", Action: action, ResourceType: "system_update", ResourceID: state.JobID,
			Metadata: map[string]any{"current_version": state.CurrentVersion, "target_version": state.TargetVersion, "rollback": state.Rollback}}
		if state.Error != "" {
			event.Metadata["error"] = state.Error
		}
		if err := s.store.WriteAudit(ctx, event); err != nil {
			s.logger.Error("reconcile portable update audit", "job_id", state.JobID, "error", err)
			continue
		}
		_, _ = s.events.Publish(ctx, Event{Type: action, ResourceType: "system_update", ResourceID: state.JobID,
			Data: map[string]any{"job_id": state.JobID, "status": state.Status, "target_version": state.TargetVersion, "rollback": state.Rollback}}, true)
		if err := s.updates.MarkReconciled(state.JobID); err != nil {
			s.logger.Error("mark portable update reconciled", "job_id", state.JobID, "error", err)
		}
	}
}
