package control

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleSetScheduleEnabled(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", "enabled is required")
		return
	}
	instanceID := chi.URLParam(r, "instanceID")
	scheduleID := chi.URLParam(r, "scheduleID")
	item, err := s.store.SetScheduleEnabled(r.Context(), instanceID, scheduleID, *request.Enabled)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "schedule.updated", "schedule", scheduleID, map[string]any{"instance_id": instanceID, "enabled": item.Enabled})
	writeJSON(w, http.StatusOK, map[string]any{"schedule": item})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	scheduleID := chi.URLParam(r, "scheduleID")
	if err := s.store.DeleteSchedule(r.Context(), instanceID, scheduleID); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "schedule.deleted", "schedule", scheduleID, map[string]any{"instance_id": instanceID})
	w.WriteHeader(http.StatusNoContent)
}
