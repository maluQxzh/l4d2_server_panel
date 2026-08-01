package control

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleListNodeQuarantine(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListQuarantineEntries(r.Context(), chi.URLParam(r, "nodeID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateQuarantinePurgeConfirmation(w http.ResponseWriter, r *http.Request) {
	nodeID, entryID := chi.URLParam(r, "nodeID"), chi.URLParam(r, "entryID")
	principal := principalFromContext(r.Context())
	token, expiresAt, err := s.store.CreateQuarantinePurgeConfirmation(r.Context(), nodeID, entryID, principal.User.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "node.quarantine.purge_confirmation_created", "node", nodeID, map[string]any{
		"quarantine_entry_id": entryID,
		"expires_at":          expiresAt,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"confirmation_token": token, "expires_at": expiresAt})
}

func (s *Server) handlePurgeQuarantineEntry(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ConfirmationToken string `json:"confirmation_token"`
	}
	if !decodeJSONWithLimit(w, r, &request, 4<<10) {
		return
	}
	request.ConfirmationToken = strings.TrimSpace(request.ConfirmationToken)
	if request.ConfirmationToken == "" {
		writeError(w, http.StatusBadRequest, "confirmation_required", "confirmation_token is required")
		return
	}
	nodeID, entryID := chi.URLParam(r, "nodeID"), chi.URLParam(r, "entryID")
	if s.agents.Connected(nodeID) && !s.agents.Supports(nodeID, "purge_quarantine") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support confirmed quarantine purge")
		return
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "quarantine", nodeID, "purge", map[string]any{
		"entry_id": entryID, "confirmation_token": request.ConfirmationToken,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueQuarantinePurgeWithFingerprint(r.Context(), nodeID, entryID, principal.User.ID,
		request.ConfirmationToken, r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principal.AllowsJob(job) {
		writeTokenScopeDenied(w)
		return
	}
	s.audit(r, "node.quarantine.purge_requested", "node", nodeID, map[string]any{
		"quarantine_entry_id": entryID,
		"job_id":              job.ID,
		"idempotent_replay":   reused,
	})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID,
		Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(nodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}
