package control

import (
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handlePackageVersions(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListPackageVersions(r.Context(), chi.URLParam(r, "instanceID"), chi.URLParam(r, "packageID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	packageID := chi.URLParam(r, "packageID")
	principal := principalFromContext(r.Context())
	idempotencyKey := r.Header.Get("Idempotency-Key")
	allowChangedFiles, _ := strconv.ParseBool(r.URL.Query().Get("allow_changed_files"))
	preserveChangedConfig, _ := strconv.ParseBool(r.URL.Query().Get("preserve_changed_config"))
	if !allowChangedFiles {
		preserveChangedConfig = false
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "package", instanceID, "delete_package", map[string]any{
		"package_id":              packageID,
		"allow_changed_files":     allowChangedFiles,
		"preserve_changed_config": preserveChangedConfig,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if idempotencyKey != "" {
		existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, requestFingerprint)
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
	pkg, path, snapshots, err := s.store.PackageForDelete(r.Context(), instanceID, packageID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !validPackageSHA(pkg.SHA256) {
		writeError(w, http.StatusConflict, "package_hash_missing", "the package has no verified SHA-256 and cannot be deleted safely")
		return
	}
	component := strings.EqualFold(pkg.Kind, "sourcemod") || strings.EqualFold(pkg.Kind, "metamod")
	var componentFiles []domain.AddonPackageFile
	if component {
		_, componentFiles, err = s.store.PackageDetails(r.Context(), instanceID, packageID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(componentFiles) == 0 {
			writeError(w, http.StatusConflict, "component_inventory_required", "该整套组件是旧版记录，请先重新安装一次以建立安全卸载清单")
			return
		}
	} else if !strings.EqualFold(pkg.Kind, "smart") {
		if err := validateDeletablePackagePath(pkg.Kind, path); err != nil {
			writeError(w, http.StatusBadRequest, "unsupported_package_delete", err.Error())
			return
		}
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if (strings.EqualFold(pkg.Kind, "smart") || component) && instance.Status != "stopped" {
		writeError(w, http.StatusConflict, "instance_must_be_stopped", "智能插件或服务端整套组件删除前必须先停止实例")
		return
	}
	if strings.EqualFold(pkg.Kind, "metamod") {
		sourceModInstalled, dependencyErr := s.store.HasPackageKind(r.Context(), instanceID, "sourcemod")
		if dependencyErr != nil {
			s.fail(w, dependencyErr)
			return
		}
		if sourceModInstalled {
			writeError(w, http.StatusConflict, "sourcemod_depends_on_metamod", "请先删除 SourceMod，再删除 Metamod:Source")
			return
		}
	}
	if strings.EqualFold(pkg.Kind, "smart") && s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "smart_plugin_archive_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target Agent does not support smart plugin deletion")
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "package_delete_v1") {
		writeError(w, http.StatusConflict, "package_delete_not_supported", "target node Agent does not support managed package deletion")
		return
	}
	if component && s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "component_package_delete_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "目标 Agent 尚不支持整套服务端组件删除")
		return
	}
	payload := map[string]any{
		"package_id": packageID, "kind": pkg.Kind, "package_path": path,
		"expected_sha256": strings.ToLower(pkg.SHA256), "snapshot_paths": snapshots,
	}
	// These flags are accepted only as explicit query options so ordinary
	// deletion remains fail-closed. The UI sets allow_changed_files after the
	// user confirms that modified files may be removed.
	if allowChangedFiles {
		payload["allow_changed_files"] = true
	}
	if preserveChangedConfig {
		payload["preserve_changed_config"] = true
	}
	if component {
		managedFiles := make([]map[string]any, 0, len(componentFiles))
		for _, file := range componentFiles {
			managedFiles = append(managedFiles, map[string]any{"path": file.Path, "sha256": file.SHA256, "size": file.Size})
		}
		payload["managed_files"] = managedFiles
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "delete_package", payload,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_package_delete", err.Error())
		}
		return
	}
	s.audit(r, "package.delete.requested", "package", packageID, map[string]any{"instance_id": instanceID, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func validateDeletablePackagePath(kind, path string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path))))
	base := filepath.Base(filepath.FromSlash(cleaned))
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "vpk", "workshop":
		if !strings.HasSuffix(strings.ToLower(base), ".vpk") || strings.HasPrefix(strings.ToLower(base), "l4d2panel-map-") {
			return errors.New("only VPK package files can be deleted")
		}
		enabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", base))
		disabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", "panel-disabled", base))
		if cleaned != enabled && cleaned != disabled {
			return errors.New("VPK package path is outside the approved package directories")
		}
	case "plugin", "smx", "sourcemod":
		if !strings.HasSuffix(strings.ToLower(base), ".smx") {
			return errors.New("only SourceMod SMX package files can be deleted")
		}
		enabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", base))
		disabled := filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", "disabled", base))
		if cleaned != enabled && cleaned != disabled {
			return errors.New("SourceMod package path is outside the approved package directories")
		}
	default:
		return errors.New("package deletion supports VPK and SourceMod SMX packages")
	}
	return nil
}

func (s *Server) handleRollbackPackageVersion(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	packageID := chi.URLParam(r, "packageID")
	versionID := chi.URLParam(r, "versionID")
	principal := principalFromContext(r.Context())
	idempotencyKey := r.Header.Get("Idempotency-Key")
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "package-version", instanceID, "rollback_package", map[string]any{
		"package_id": packageID, "version_id": versionID,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if idempotencyKey != "" {
		existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, requestFingerprint)
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
	pkg, version, err := s.store.PackageVersionForRollback(r.Context(), instanceID, packageID, versionID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !pkg.Enabled {
		writeError(w, http.StatusConflict, "package_disabled", "enable the package before rolling back its version")
		return
	}
	if pkg.Path != version.TargetPath {
		writeError(w, http.StatusConflict, "package_path_changed", "the saved version no longer targets the installed package path")
		return
	}
	if !validPackageSHA(version.SHA256) || strings.EqualFold(pkg.SHA256, version.SHA256) {
		writeError(w, http.StatusConflict, "package_version_current", "the selected package version is already installed or invalid")
		return
	}
	if err := validateVersionedPackageResultPath(pkg.Kind, pkg.Path); err != nil {
		writeError(w, http.StatusBadRequest, "unsupported_package_rollback", err.Error())
		return
	}
	payload := map[string]any{
		"package_id":      packageID,
		"version_id":      versionID,
		"kind":            pkg.Kind,
		"package_path":    pkg.Path,
		"snapshot_path":   version.StoragePath,
		"expected_sha256": version.SHA256,
		"restore_version": version.Version,
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "rollback_package", payload,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_package_rollback", err.Error())
		}
		return
	}
	s.audit(r, "package.rollback.requested", "package", packageID, map[string]any{
		"instance_id": instanceID, "version_id": versionID, "job_id": job.ID,
	})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}
