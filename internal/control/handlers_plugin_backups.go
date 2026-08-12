package control

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	pluginBackupLayoutPreserve = "preserve"
	pluginBackupLayoutCompact  = "compact"
	maxPluginBackupPackages    = 1000
	maxPluginBackupFiles       = 5000
)

type createPluginBackupRequest struct {
	Layout string `json:"layout"`
}

func (s *Server) handleCreatePluginBackup(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	var request createPluginBackupRequest
	if !decodeJSONWithLimit(w, r, &request, 8<<10) {
		return
	}
	if request.Layout != pluginBackupLayoutPreserve && request.Layout != pluginBackupLayoutCompact {
		writeError(w, http.StatusBadRequest, "invalid_plugin_backup_layout", "layout must be preserve or compact")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "plugin_backup_7z_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "目标 Agent 尚不支持插件 7z 备份")
		return
	}
	packages, err := s.store.PluginBackupPackages(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	filtered := make([]domain.PluginBackupPackage, 0, len(packages))
	fileCount := 0
	for _, item := range packages {
		if len(item.Files) == 0 {
			continue
		}
		fileCount += len(item.Files)
		filtered = append(filtered, item)
	}
	if len(filtered) == 0 {
		writeError(w, http.StatusConflict, "no_plugin_files", "当前实例没有可备份的插件文件")
		return
	}
	if len(filtered) > maxPluginBackupPackages || fileCount > maxPluginBackupFiles {
		writeError(w, http.StatusConflict, "plugin_backup_too_large", "插件清单超出单次备份限制")
		return
	}
	payload := map[string]any{"layout": request.Layout, "plugin_packages": filtered}
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-backup", instanceID, "plugin", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, fingerprint); lookupErr == nil {
		s.dispatchSoon(existing.NodeID)
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": existing.ID, "idempotent_replay": true})
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.fail(w, lookupErr)
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "plugin.backup", payload,
		principal.User.ID, idempotencyKey, fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.plugin_backup.create_requested", "instance", instanceID, map[string]any{
		"layout": request.Layout, "package_count": len(filtered), "file_count": fileCount, "job_id": job.ID,
	})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID,
		Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused,
		"message": fmt.Sprintf("已提交 %d 个插件的备份任务", len(filtered))})
}
