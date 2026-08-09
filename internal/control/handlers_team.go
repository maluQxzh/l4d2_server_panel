package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// updateTeamUserAndInvalidate keeps authorization invalidation in the Control
// layer and deliberately bumps the generation whenever the store call returns.
// In particular, UpdateTeamUser may have committed before its final user reload
// fails. Conservatively invalidating on earlier failures is harmless and avoids
// exposing transaction internals through the Store API.
func (s *Server) updateTeamUserAndInvalidate(ctx context.Context, actorID, userID string, input UpdateTeamUserInput,
	update func(context.Context, string, string, UpdateTeamUserInput) (domain.User, error),
) (domain.User, error) {
	updated, err := update(ctx, actorID, userID, input)
	s.events.InvalidateAuthorization()
	return updated, err
}

type roleBindingRequest struct {
	Role        domain.Role     `json:"role"`
	Permissions json.RawMessage `json:"permissions,omitempty"`
	ScopeType   string          `json:"scope_type"`
	NodeID      string          `json:"node_id,omitempty"`
	InstanceID  string          `json:"instance_id,omitempty"`
}

func parseRoleBindingRequests(raw json.RawMessage) ([]roleBindingRequest, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true, errors.New("role_bindings must contain between 1 and 500 bindings")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var requests []roleBindingRequest
	if err := decoder.Decode(&requests); err != nil {
		return nil, true, errors.New("role_bindings must be an array of bindings")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, true, errors.New("role_bindings must be an array of bindings")
	}
	if len(requests) == 0 {
		return nil, true, errors.New("role_bindings must contain between 1 and 500 bindings")
	}
	return requests, true, nil
}

// requireAccountSecurity is a defence-in-depth check for handlers that expose
// account data or mutate credentials and role bindings. Routes apply the same
// owner-only permission before invoking a handler, but keeping the check here
// prevents a future route or direct handler call from widening this boundary.
func requireAccountSecurity(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	principal := principalFromContext(r.Context())
	if !principal.GlobalAllows(PermissionRootSecurity) {
		writeError(w, http.StatusForbidden, "owner_permission_required", "only an owner can manage accounts")
		return Principal{}, false
	}
	return principal, true
}

func bindingsFromRequest(role domain.Role, requests []roleBindingRequest) ([]domain.RoleBinding, error) {
	if len(requests) == 0 {
		if !validRole(role) {
			return nil, errors.New("role or role_bindings is required")
		}
		return []domain.RoleBinding{{Role: role, ScopeType: "global"}}, nil
	}
	bindings := make([]domain.RoleBinding, 0, len(requests))
	for _, request := range requests {
		var permissions []string
		if request.Permissions != nil {
			if strings.TrimSpace(string(request.Permissions)) == "null" {
				permissions = make([]string, 0)
			} else if err := json.Unmarshal(request.Permissions, &permissions); err != nil {
				return nil, errors.New("permissions must be an array of permission names")
			}
			if permissions == nil {
				permissions = make([]string, 0)
			}
		}
		bindings = append(bindings, domain.RoleBinding{Role: request.Role, Permissions: permissions, ScopeType: request.ScopeType,
			NodeID: request.NodeID, InstanceID: request.InstanceID})
	}
	return normalizeRoleBindings(bindings)
}

func actorCanAssignBindings(principal Principal, bindings []domain.RoleBinding) bool {
	for _, binding := range bindings {
		if binding.Role == domain.RoleOwner {
			if !principal.GlobalAllows(PermissionRootSecurity) {
				return false
			}
			continue
		}
		if !principal.GlobalAllows(PermissionManage) {
			return false
		}
	}
	return true
}

func userHasOwnerBinding(user domain.User) bool {
	if user.Role == domain.RoleOwner {
		return true
	}
	for _, binding := range user.RoleBindings {
		if binding.Role == domain.RoleOwner {
			return true
		}
	}
	return false
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	principal, allowed := requireAccountSecurity(w, r)
	if !allowed {
		return
	}
	var request struct {
		Username     string          `json:"username"`
		DisplayName  string          `json:"display_name,omitempty"`
		Role         domain.Role     `json:"role,omitempty"`
		RoleBindings json.RawMessage `json:"role_bindings,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	roleBindings, _, err := parseRoleBindingRequests(request.RoleBindings)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_role_bindings", err.Error())
		return
	}
	bindings, err := bindingsFromRequest(request.Role, roleBindings)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_role_bindings", err.Error())
		return
	}
	if !actorCanAssignBindings(principal, bindings) {
		writeError(w, http.StatusForbidden, "owner_permission_required", "only an owner can create another owner")
		return
	}
	user, credential, err := s.store.CreateTeamUser(r.Context(), principal.User.ID, CreateTeamUserInput{Username: request.Username,
		DisplayName: request.DisplayName, Bindings: bindings})
	if err != nil {
		if errors.Is(err, errRootSecurityRequired) {
			writeError(w, http.StatusForbidden, "owner_permission_required", "only an owner can manage accounts")
			return
		}
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, "username_conflict", "username already exists")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_user", err.Error())
		return
	}
	s.audit(r, "user.created", "user", user.ID, map[string]any{"username": user.Username, "role_bindings": user.RoleBindings})
	writeJSON(w, http.StatusCreated, map[string]any{"user": user, "one_time_password": credential.OneTimePassword, "expires_at": credential.ExpiresAt})
}

func (s *Server) handleReissueUserActivation(w http.ResponseWriter, r *http.Request) {
	principal, allowed := requireAccountSecurity(w, r)
	if !allowed {
		return
	}
	userID := chi.URLParam(r, "userID")
	credential, err := s.store.ReissueUserActivation(r.Context(), principal.User.ID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "user_not_found", "user was not found")
			return
		}
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, "account_already_active", "only a pending account can receive a new one-time password")
			return
		}
		if errors.Is(err, errRootSecurityRequired) {
			writeError(w, http.StatusForbidden, "owner_permission_required", "only an owner can manage accounts")
			return
		}
		s.fail(w, err)
		return
	}
	s.events.InvalidateAuthorization()
	s.audit(r, "user.activation_reissued", "user", userID, map[string]any{"expires_at": credential.ExpiresAt})
	writeJSON(w, http.StatusCreated, credential)
}

func (s *Server) handleUserRoleBindings(w http.ResponseWriter, r *http.Request) {
	if _, allowed := requireAccountSecurity(w, r); !allowed {
		return
	}
	user, err := s.store.TeamUser(r.Context(), chi.URLParam(r, "userID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": user.RoleBindings})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	principal, allowed := requireAccountSecurity(w, r)
	if !allowed {
		return
	}
	var request struct {
		DisplayName  *string         `json:"display_name,omitempty"`
		Password     *string         `json:"password,omitempty"`
		Disabled     *bool           `json:"disabled,omitempty"`
		Role         *domain.Role    `json:"role,omitempty"`
		RoleBindings json.RawMessage `json:"role_bindings,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	roleBindingsProvided := request.RoleBindings != nil
	if request.DisplayName == nil && request.Password == nil && request.Disabled == nil && request.Role == nil && !roleBindingsProvided {
		writeError(w, http.StatusBadRequest, "invalid_user_update", "at least one field must be provided")
		return
	}
	if request.Role != nil && roleBindingsProvided {
		writeError(w, http.StatusBadRequest, "invalid_role_bindings", "provide role or role_bindings, not both")
		return
	}
	roleBindings, _, err := parseRoleBindingRequests(request.RoleBindings)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_role_bindings", err.Error())
		return
	}
	userID := chi.URLParam(r, "userID")
	current, err := s.store.TeamUser(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if request.Password != nil && current.AccountState == domain.AccountPendingActivation {
		writeError(w, http.StatusConflict, "activation_pending", "reissue the one-time password for a pending account instead of setting its password")
		return
	}
	if userHasOwnerBinding(current) && !principal.GlobalAllows(PermissionRootSecurity) {
		writeError(w, http.StatusForbidden, "owner_permission_required", "administrators cannot modify an owner")
		return
	}
	if request.Disabled != nil && *request.Disabled && userID == principal.User.ID {
		writeError(w, http.StatusConflict, "cannot_disable_self", "the current account cannot disable itself")
		return
	}
	input := UpdateTeamUserInput{DisplayName: request.DisplayName, Disabled: request.Disabled}
	if request.Password != nil {
		hash, hashErr := HashPassword(*request.Password)
		if hashErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_password", hashErr.Error())
			return
		}
		input.PasswordHash = &hash
	}
	if request.Role != nil || roleBindingsProvided {
		var raw []roleBindingRequest
		role := domain.Role("")
		if request.Role != nil {
			role = *request.Role
		} else {
			raw = roleBindings
		}
		bindings, bindingErr := bindingsFromRequest(role, raw)
		if bindingErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_role_bindings", bindingErr.Error())
			return
		}
		if !actorCanAssignBindings(principal, bindings) {
			writeError(w, http.StatusForbidden, "owner_permission_required", "only an owner can assign the owner role")
			return
		}
		input.Bindings = &bindings
	}
	updated, err := s.updateTeamUserAndInvalidate(r.Context(), principal.User.ID, userID, input, s.store.UpdateTeamUser)
	if err != nil {
		if errors.Is(err, errRootSecurityRequired) {
			writeError(w, http.StatusForbidden, "owner_permission_required", "only an owner can manage accounts")
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "owner") {
			status = http.StatusConflict
		}
		writeError(w, status, "invalid_user_update", err.Error())
		return
	}
	metadata := map[string]any{"disabled": updated.Disabled, "role_bindings": updated.RoleBindings}
	if request.Password != nil {
		metadata["password_reset"] = true
	}
	s.audit(r, "user.updated", "user", updated.ID, metadata)
	writeJSON(w, http.StatusOK, map[string]any{"user": updated})
}
