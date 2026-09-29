package control

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// This deliberately returns only the uploader's import outcome, without
// granting access to general job details, payloads, or other users' imports.
func (s *Server) handleGetMapImport(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "importID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if job.Type != "map.import.qq_flash" || job.RequestedBy != principal.User.ID || job.NodeID == "" {
		writeError(w, http.StatusNotFound, "not_found", "map import not found")
		return
	}
	allowed, err := s.allowsMapNodePermission(r.Context(), principal, job.NodeID, PermissionMapsImport)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !allowed {
		writeTokenScopeDenied(w)
		return
	}
	ready := settledTerminalJob(job) && !jobProjectionPending(job)
	result := map[string]any{"job_id": job.ID, "status": job.Status, "ready": ready}
	if ready && job.Status == domain.JobSucceeded {
		warnings, err := mapWarningsFromResult(job.Result["warnings"])
		if err != nil {
			s.fail(w, err)
			return
		}
		if warnings == nil {
			warnings = []domain.MapWarning{}
		}
		bsps, err := mapBSPsFromResult(job.Result["bsps"])
		if err != nil {
			s.fail(w, err)
			return
		}
		missing := make([]string, 0)
		for _, bsp := range bsps {
			if !bsp.DictionaryPresent {
				missing = append(missing, bsp.VirtualPath)
			}
		}
		duplicate, ok := job.Result["import_duplicate"].(bool)
		if !ok { // Compatibility with tasks completed before outcome projection.
			duplicate = stringFromAny(job.Result["map_id"]) != stringFromAny(job.Payload["map_id"])
		}
		result["name"] = stringFromAny(job.Result["name"])
		result["duplicate"] = duplicate
		result["warnings"] = warnings
		result["missing_dictionaries"] = missing
	}
	writeJSON(w, http.StatusOK, result)
}
