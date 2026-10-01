package control

import (
	"database/sql"
	"errors"
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
		if duplicate {
			allowed, err := s.allowsMapNodePermission(r.Context(), principal, job.NodeID, PermissionMapsRead)
			if err != nil {
				s.fail(w, err)
				return
			}
			if allowed {
				// Canonical IDs are recorded by Control after either deduplication path.
				// Legacy results may only have the Agent ID and aggregate digest.
				mapID := stringFromAny(job.Result["import_map_id"])
				legacy := mapID == ""
				if legacy {
					mapID = stringFromAny(job.Result["map_id"])
				}
				item, err := scanMap(s.store.db.QueryRowContext(r.Context(), `SELECT `+mapColumns+` FROM maps
					WHERE node_id=?1 AND (id=?2 OR (?3 AND aggregate_sha256=?4 AND health<>'deleting'))
					ORDER BY CASE WHEN id=?2 THEN 0 ELSE 1 END,created_at,id LIMIT 1`,
					job.NodeID, mapID, legacy, stringFromAny(job.Result["aggregate_sha256"])))
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					s.fail(w, err)
					return
				}
				if err == nil {
					result["existing_map"] = map[string]any{
						"id": item.ID, "name": item.Name, "created_by_display_name": item.CreatedByName,
						"created_at": item.CreatedAt, "source_name": item.SourceName,
						"author": item.Author, "version": item.Version, "total_size": item.TotalSize,
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, result)
}
