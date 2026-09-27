package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func (s *Server) handleCreatePluginArchiveInspection(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name            string `json:"name"`
		ContentBase64   string `json:"content_base64"`
		Source          string `json:"source"`
		SourceTransport string `json:"source_transport"`
		PathEncoding    string `json:"path_encoding"`
	}
	if !decodeJSONWithLimit(w, r, &request, protocol.MaxPackageUploadEnvelopeBytes) {
		return
	}
	pathEncoding, err := normalizePluginArchivePathEncoding(request.PathEncoding)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_plugin_path_encoding", err.Error())
		return
	}
	payload := map[string]any{
		"name": strings.TrimSpace(request.Name), "content_base64": request.ContentBase64,
		"source": strings.TrimSpace(request.Source), "source_transport": strings.TrimSpace(request.SourceTransport),
		"path_encoding": pathEncoding,
	}
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
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, protocol.CapabilitySmartPluginArchiveV2) {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "目标 Agent 不支持插件压缩包路径编码，请先升级 Agent")
		return
	}
	if strings.EqualFold(request.SourceTransport, "qq_flash") && s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "qq_flash_plugin_packages_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target Agent does not support QQ flash plugin packages")
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
	s.audit(r, "plugin_archive.inspection.requested", "instance", instanceID, map[string]any{"job_id": job.ID, "name": request.Name, "path_encoding": pathEncoding})
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
	if stringFromAny(payload["source"]) != "" && !strings.EqualFold(stringFromAny(payload["source_transport"]), "qq_flash") {
		return errors.New("smart plugin installation accepts local uploads or QQ flash archives only")
	}
	pathEncoding, err := normalizePluginArchivePathEncoding(stringFromAny(payload["path_encoding"]))
	if err != nil {
		return err
	}
	payload["path_encoding"] = pathEncoding
	fingerprint := strings.ToLower(strings.TrimSpace(stringFromAny(payload["inspection_fingerprint"])))
	if len(fingerprint) != 64 || strings.Trim(fingerprint, "0123456789abcdef") != "" {
		return errors.New("smart plugin installation requires a valid inspection fingerprint")
	}
	packages, ok := payload["smart_packages"].([]any)
	if !ok || len(packages) == 0 || len(packages) > 256 {
		return errors.New("smart plugin installation requires 1 to 256 selected folders")
	}
	raw, err := json.Marshal(packages)
	var selections []preparedSmartSelection
	if err != nil || json.Unmarshal(raw, &selections) != nil {
		return errors.New("invalid smart plugin selections")
	}
	for _, selection := range selections {
		seen := map[string]bool{}
		if len(selection.Takeovers)+len(selection.PreserveExisting) > 5000 || (selection.PackageID != "" && len(selection.PreserveExisting) != 0) {
			return errors.New("invalid preserve-existing selections")
		}
		for _, ids := range [][]string{selection.Takeovers, selection.PreserveExisting} {
			for _, id := range ids {
				if id == "" || len(id) > 128 || seen[id] {
					return errors.New("missing, duplicate or conflicting takeover selection")
				}
				seen[id] = true
			}
		}
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

func smartPluginPreserveRequested(payload map[string]any) bool {
	raw, err := json.Marshal(payload["smart_packages"])
	var selections []preparedSmartSelection
	if err != nil || json.Unmarshal(raw, &selections) != nil {
		return false
	}
	for _, selection := range selections {
		if len(selection.PreserveExisting) > 0 {
			return true
		}
	}
	return false
}

func normalizePluginArchivePathEncoding(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = domain.MapPathEncodingAuto
	}
	if !domain.ValidMapPathEncoding(value) {
		return "", errors.New("path_encoding 必须是 auto、utf8、gbk 或 big5")
	}
	return value, nil
}
