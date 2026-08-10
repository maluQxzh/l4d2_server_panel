package control

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func (s *Server) handleCreatePluginArchiveInspection(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name          string `json:"name"`
		ContentBase64 string `json:"content_base64"`
	}
	if !decodeJSONWithLimit(w, r, &request, protocol.MaxPackageUploadEnvelopeBytes) {
		return
	}
	payload := map[string]any{"name": strings.TrimSpace(request.Name), "content_base64": request.ContentBase64}
	if err := validatePackageUpload(payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_plugin_archive", err.Error())
		return
	}
	instanceID := chi.URLParam(r, "instanceID")
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	managedPaths, err := s.store.ManagedPackagePaths(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	payload["registered_paths"] = managedPaths
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "smart_plugin_archive_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target Agent does not support smart plugin archive inspection")
		return
	}
	principal := principalFromContext(r.Context())
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "plugin-archive-inspection", instanceID, "inspect_plugin_archive", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "inspect_plugin_archive", payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_plugin_archive", err.Error())
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "inspect_plugin_archive", payload,
		principal.User.ID, r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "plugin_archive.inspection.requested", "instance", instanceID, map[string]any{"job_id": job.ID, "name": request.Name})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleGetPluginArchiveInspection(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if job.InstanceID != chi.URLParam(r, "instanceID") || job.Type != "inspect_plugin_archive" {
		s.fail(w, ErrNotFound)
		return
	}
	response := map[string]any{"job_id": job.ID, "status": job.Status, "progress": job.Progress, "message": job.Message}
	if job.Status == domain.JobSucceeded {
		response["plan"] = job.Result
	}
	if job.Status == domain.JobFailed {
		response["error"] = job.Message
	}
	writeJSON(w, http.StatusOK, response)
}

func validateSmartPluginInstallPayload(instance domain.Instance, payload map[string]any) error {
	if !strings.EqualFold(stringFromAny(payload["kind"]), "smart") {
		return nil
	}
	if stringFromAny(payload["source"]) != "" {
		return errors.New("smart plugin installation accepts local archives only")
	}
	fingerprint := strings.ToLower(strings.TrimSpace(stringFromAny(payload["inspection_fingerprint"])))
	if len(fingerprint) != 64 || strings.Trim(fingerprint, "0123456789abcdef") != "" {
		return errors.New("smart plugin installation requires a valid inspection fingerprint")
	}
	packages, ok := payload["smart_packages"].([]any)
	if !ok || len(packages) == 0 || len(packages) > 256 {
		return errors.New("smart plugin installation requires 1 to 256 selected folders")
	}
	if instance.Status == domain.InstanceRunning || instance.Status == domain.InstanceDegraded || instance.Status == domain.InstanceExternal {
		live, _ := payload["acknowledge_live_changes"].(bool)
		restart, _ := payload["acknowledge_restart_required"].(bool)
		if !live || !restart {
			return errors.New("running-instance smart plugin installation requires both live-change warnings to be acknowledged")
		}
	}
	return nil
}
