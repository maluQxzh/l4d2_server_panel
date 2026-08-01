package control

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	items, err := s.store.ListAPITokens(r.Context(), principal.User.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string     `json:"name"`
		Scopes     []string   `json:"scopes"`
		NodeID     string     `json:"node_id,omitempty"`
		InstanceID string     `json:"instance_id,omitempty"`
		ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if len(request.Name) == 0 || len(request.Name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_api_token", "name must be between 1 and 100 bytes")
		return
	}
	scopes, err := normalizeTokenScopes(request.Scopes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_api_token", err.Error())
		return
	}
	principal := principalFromContext(r.Context())
	if err := validateTokenScopesForRole(principal.User.Role, scopes); err != nil {
		writeError(w, http.StatusForbidden, "scope_exceeds_role", err.Error())
		return
	}
	if request.ExpiresAt == nil {
		writeError(w, http.StatusBadRequest, "invalid_api_token", "expires_at is required")
		return
	}
	expires := request.ExpiresAt.UTC()
	if !expires.After(time.Now().UTC()) {
		writeError(w, http.StatusBadRequest, "invalid_api_token", "expires_at must be in the future")
		return
	}
	request.ExpiresAt = &expires
	request.NodeID = strings.TrimSpace(request.NodeID)
	request.InstanceID = strings.TrimSpace(request.InstanceID)
	if err := s.store.ValidateAPITokenResources(r.Context(), request.NodeID, request.InstanceID); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, "invalid_resource_scope", err.Error())
		return
	}
	for _, scope := range scopes {
		permission := Permission(scope)
		allowed := false
		if request.InstanceID != "" {
			instance, lookupErr := s.store.GetInstance(r.Context(), request.InstanceID)
			if lookupErr != nil {
				s.fail(w, lookupErr)
				return
			}
			allowed = principal.AllowsInstancePermission(instance, permission)
		} else if request.NodeID != "" {
			allowed = principal.AllowsNodePermission(request.NodeID, permission)
		} else {
			allowed = principal.GlobalAllows(permission)
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "scope_exceeds_binding", "API token scope exceeds the user's role binding for this resource")
			return
		}
	}
	secret, err := newAPITokenSecret()
	if err != nil {
		s.fail(w, err)
		return
	}
	token, err := s.store.CreateAPIToken(r.Context(), APIToken{UserID: principal.User.ID, Name: request.Name,
		Scopes: scopes, NodeID: request.NodeID, InstanceID: request.InstanceID, ExpiresAt: request.ExpiresAt}, tokenHash(secret))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_api_token", err.Error())
		return
	}
	s.audit(r, "api_token.created", "api_token", token.ID, map[string]any{"name": token.Name, "scopes": token.Scopes,
		"node_id": token.NodeID, "instance_id": token.InstanceID, "expires_at": token.ExpiresAt})
	// The high-entropy bearer secret is deliberately returned by this response
	// only. Lists and audit entries contain metadata, never the secret or hash.
	writeJSON(w, http.StatusCreated, map[string]any{"api_token": token, "token": secret})
}

func (s *Server) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	id := chi.URLParam(r, "tokenID")
	if err := s.store.RevokeAPIToken(r.Context(), principal.User.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	s.events.InvalidateAuthorization()
	s.audit(r, "api_token.revoked", "api_token", id, nil)
	w.WriteHeader(http.StatusNoContent)
}
