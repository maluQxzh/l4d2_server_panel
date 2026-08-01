package control

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handlePackageVersions(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListPackageVersions(r.Context(), chi.URLParam(r, "instanceID"), chi.URLParam(r, "packageID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
