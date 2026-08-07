package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleListMaps(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	nodeID := strings.TrimSpace(r.URL.Query().Get("node_id"))
	if nodeID != "" && !principal.AllowsNodePermission(nodeID, PermissionRead) {
		writeTokenScopeDenied(w)
		return
	}
	limit := parseLimit(r, 200)
	queryLimit := limit
	if nodeID == "" && principal.HasResourceRestriction() {
		queryLimit = min(500, limit*5)
	}
	items, err := s.store.ListMaps(r.Context(), nodeID, queryLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	filtered := make([]domain.Map, 0, min(limit, len(items)))
	for _, item := range items {
		if principal.AllowsNodePermission(item.NodeID, PermissionRead) {
			filtered = append(filtered, item)
			if len(filtered) == limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": filtered})
}

func (s *Server) handleGetMap(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetMap(r.Context(), chi.URLParam(r, "mapID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principalFromContext(r.Context()).AllowsNodePermission(item.NodeID, PermissionRead) {
		writeTokenScopeDenied(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"map": item})
}

func (s *Server) handleGetMapPoster(w http.ResponseWriter, r *http.Request) {
	nodeID, mime, data, err := s.store.GetMapPoster(r.Context(), chi.URLParam(r, "mapID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principalFromContext(r.Context()).AllowsNodePermission(nodeID, PermissionRead) {
		writeTokenScopeDenied(w)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleImportMap(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ShareURL string `json:"share_url"`
	}
	if !decodeJSONWithLimit(w, r, &request, 4<<10) {
		return
	}
	shareURL, err := normalizeQQFlashShareURL(request.ShareURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_qq_flash_share_url", err.Error())
		return
	}
	nodeID := chi.URLParam(r, "nodeID")
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(nodeID, PermissionManage) {
		writeError(w, http.StatusForbidden, "forbidden", "map import permission denied")
		return
	}
	if s.agents.Connected(nodeID) && !s.agents.Supports(nodeID, "map_library_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target node Agent does not support the map library")
		return
	}
	fingerprintPayload := map[string]any{"source": shareURL}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "node-map", nodeID, "import", fingerprintPayload)
	if err != nil {
		s.fail(w, err)
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
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
	payload := map[string]any{"source": shareURL, "map_id": uuid.NewString()}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "map.import.qq_flash", payload); err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueNodeJobWithFingerprint(r.Context(), nodeID, "map.import.qq_flash", payload,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "map.import.requested", "node", nodeID, map[string]any{"job_id": job.ID})
	s.publishMapJob(r, job)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleAssignMap(w http.ResponseWriter, r *http.Request) {
	s.handleMapAssignment(w, r, "map.assign")
}

func (s *Server) handleUnassignMap(w http.ResponseWriter, r *http.Request) {
	s.handleMapAssignment(w, r, "map.unassign")
}

func (s *Server) handleMapAssignment(w http.ResponseWriter, r *http.Request, action string) {
	mapID, instanceID := chi.URLParam(r, "mapID"), chi.URLParam(r, "instanceID")
	item, err := s.store.GetMap(r.Context(), mapID)
	if err != nil {
		s.fail(w, err)
		return
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(item.NodeID, PermissionRead) || !principal.AllowsInstancePermission(instance, PermissionManage) {
		writeError(w, http.StatusForbidden, "forbidden", "map assignment permission denied")
		return
	}
	if item.NodeID != instance.NodeID {
		writeError(w, http.StatusConflict, "cross_node_map_assignment", "map and instance must belong to the same node")
		return
	}
	if s.agents.Connected(item.NodeID) && !s.agents.Supports(item.NodeID, "map_library_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target node Agent does not support the map library")
		return
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "map-assignment", mapID, action,
		map[string]any{"instance_id": instanceID})
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueMapInstanceJob(r.Context(), mapID, instanceID, action, principal.User.ID,
		r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, action+".requested", "map", mapID, map[string]any{"instance_id": instanceID, "job_id": job.ID})
	s.publishMapJob(r, job)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleVerifyMap(w http.ResponseWriter, r *http.Request) {
	mapID := chi.URLParam(r, "mapID")
	item, err := s.store.GetMap(r.Context(), mapID)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(item.NodeID, PermissionManage) {
		writeError(w, http.StatusForbidden, "forbidden", "map verification permission denied")
		return
	}
	if s.agents.Connected(item.NodeID) && !s.agents.Supports(item.NodeID, "map_library_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target node Agent does not support the map library")
		return
	}
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "map", mapID, "verify", map[string]any{})
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueMapNodeJob(r.Context(), mapID, "map.verify", principal.User.ID,
		r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "map.verify.requested", "map", mapID, map[string]any{"job_id": job.ID})
	s.publishMapJob(r, job)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleCreateMapDeleteConfirmation(w http.ResponseWriter, r *http.Request) {
	mapID := chi.URLParam(r, "mapID")
	item, err := s.store.GetMap(r.Context(), mapID)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(item.NodeID, PermissionManage) {
		writeError(w, http.StatusForbidden, "forbidden", "map deletion permission denied")
		return
	}
	token, expiresAt, err := s.store.CreateMapDeleteConfirmation(r.Context(), mapID, principal.User.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "map.delete_confirmation.created", "map", mapID, map[string]any{"expires_at": expiresAt})
	writeJSON(w, http.StatusCreated, map[string]any{"confirmation_token": token, "expires_at": expiresAt})
}

func (s *Server) handleDeleteMap(w http.ResponseWriter, r *http.Request) {
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
	mapID := chi.URLParam(r, "mapID")
	item, err := s.store.GetMap(r.Context(), mapID)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(item.NodeID, PermissionManage) {
		writeError(w, http.StatusForbidden, "forbidden", "map deletion permission denied")
		return
	}
	if s.agents.Connected(item.NodeID) && !s.agents.Supports(item.NodeID, "map_library_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "target node Agent does not support the map library")
		return
	}
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "map", mapID, "delete",
		map[string]any{"confirmation_token": request.ConfirmationToken})
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueMapDelete(r.Context(), mapID, principal.User.ID, request.ConfirmationToken,
		r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "map.delete.requested", "map", mapID, map[string]any{"job_id": job.ID})
	s.publishMapJob(r, job)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) publishMapJob(r *http.Request, job domain.Job) {
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID,
		Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
}
