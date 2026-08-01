package control

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type discoveryRequest struct {
	MaxDepth   int `json:"max_depth,omitempty"`
	MaxEntries int `json:"max_entries,omitempty"`
}

func normalizeDiscoveryRequest(request discoveryRequest) (discoveryRequest, error) {
	if request.MaxDepth == 0 {
		request.MaxDepth = 6
	}
	if request.MaxEntries == 0 {
		request.MaxEntries = 5000
	}
	if request.MaxDepth < 1 || request.MaxDepth > 8 {
		return request, errors.New("max_depth must be between 1 and 8")
	}
	if request.MaxEntries < 1 || request.MaxEntries > 20000 {
		return request, errors.New("max_entries must be between 1 and 20000")
	}
	return request, nil
}

func (s *Server) handleCreateNodeDiscovery(w http.ResponseWriter, r *http.Request) {
	var request discoveryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request, err := normalizeDiscoveryRequest(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_discovery_limits", err.Error())
		return
	}
	nodeID := chi.URLParam(r, "nodeID")
	node, err := s.store.GetNode(r.Context(), nodeID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if node.Status != domain.NodeOnline || !s.agents.Connected(nodeID) {
		writeError(w, http.StatusConflict, "node_offline", "node must be online to scan instance roots")
		return
	}
	if !s.agents.Supports(nodeID, "discover_instances") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support instance discovery")
		return
	}
	principal := principalFromContext(r.Context())
	payload := map[string]any{"max_depth": request.MaxDepth, "max_entries": request.MaxEntries}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "node-discovery", nodeID, "discover_instances", request)
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueNodeJobWithFingerprint(r.Context(), nodeID, "discover_instances", payload, principal.User.ID,
		r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "node.discovery.requested", "node", nodeID, map[string]any{"job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(nodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleGetNodeDiscovery(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if job.NodeID != chi.URLParam(r, "nodeID") || job.InstanceID != "" || job.Type != "discover_instances" {
		writeError(w, http.StatusNotFound, "not_found", "discovery task not found")
		return
	}
	principal := principalFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"job": redactJobForPrincipal(job, principal)})
}
