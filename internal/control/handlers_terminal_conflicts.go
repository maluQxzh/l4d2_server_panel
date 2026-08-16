package control

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleListTerminalOutcomeConflicts(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") != "false"
	items, err := s.store.ListTerminalOutcomeConflicts(r.Context(), activeOnly, parseLimit(r, 100))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetTerminalOutcomeConflict(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetTerminalOutcomeConflict(r.Context(), chi.URLParam(r, "jobID"),
		strings.TrimSpace(r.URL.Query().Get("reported_fingerprint")))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflict": item})
}

func (s *Server) handleResolveTerminalOutcomeConflict(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Action              string `json:"action"`
		ReportedFingerprint string `json:"reported_fingerprint"`
	}
	if !decodeJSONWithLimit(w, r, &request, 4<<10) {
		return
	}
	request.Action = strings.TrimSpace(request.Action)
	request.ReportedFingerprint = strings.TrimSpace(request.ReportedFingerprint)
	if request.Action != "keep_control" || !validRequestFingerprint(request.ReportedFingerprint) {
		writeError(w, http.StatusBadRequest, "invalid_terminal_conflict_resolution",
			"action must be keep_control and reported_fingerprint must identify the current conflict")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		writeError(w, http.StatusBadRequest, "idempotency_key_required",
			"Idempotency-Key is required and must not exceed 200 characters")
		return
	}
	jobID := chi.URLParam(r, "jobID")
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "job-terminal-conflict", jobID,
		"keep_control", map[string]any{"reported_fingerprint": request.ReportedFingerprint})
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	item, reused, err := s.store.ResolveTerminalOutcomeConflict(r.Context(), jobID, request.ReportedFingerprint,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	acknowledged, ackErr := s.agents.AcknowledgeResolvedTerminalConflict(item.NodeID, item.JobID, item.ReportedFingerprint)
	if ackErr != nil {
		s.logger.Warn("acknowledge resolved terminal conflict", "job_id", item.JobID, "node_id", item.NodeID, "error", ackErr)
	}
	if !reused {
		s.audit(r, "job.terminal_conflict.resolved", "job", item.JobID, map[string]any{
			"node_id": item.NodeID, "instance_id": item.InstanceID, "resolution": item.Resolution,
			"stored_status": item.StoredStatus, "reported_status": item.ReportedStatus,
		})
		_, _ = s.events.Publish(r.Context(), Event{Type: "job.terminal_conflict.resolved", ResourceType: "job",
			ResourceID: item.JobID, Data: map[string]any{"job_id": item.JobID, "node_id": item.NodeID,
				"instance_id": item.InstanceID, "resolution": item.Resolution}}, true)
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflict": item, "idempotent_replay": reused,
		"acknowledgement_delivered": acknowledged})
}
