package control

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListEnvironments(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	filtered := make([]domain.RuntimeEnvironment, 0, len(items))
	for _, item := range items {
		if principal.AllowsNodePermission(item.NodeID, PermissionEnvironmentsRead) {
			item.AgentSupported = s.agents.Supports(item.NodeID, "runtime_environment_v1")
			filtered = append(filtered, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": filtered})
}

func (s *Server) environmentForRequest(w http.ResponseWriter, r *http.Request, permission Permission) (domain.RuntimeEnvironment, bool) {
	item, err := s.store.GetEnvironment(r.Context(), chi.URLParam(r, "environmentID"))
	if err != nil {
		s.fail(w, err)
		return domain.RuntimeEnvironment{}, false
	}
	if !principalFromContext(r.Context()).AllowsNodePermission(item.NodeID, permission) {
		writeTokenScopeDenied(w)
		return domain.RuntimeEnvironment{}, false
	}
	item.AgentSupported = s.agents.Supports(item.NodeID, "runtime_environment_v1")
	return item, true
}

func (s *Server) handleGetEnvironment(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsRead)
	if !ok {
		return
	}
	installations, err := s.store.ListGameInstallations(r.Context(), item.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environment": item, "game_installations": installations})
}

func (s *Server) requireEnvironmentAgent(w http.ResponseWriter, item domain.RuntimeEnvironment) bool {
	if !s.agents.Connected(item.NodeID) {
		writeError(w, http.StatusConflict, "node_offline", "node must be online for runtime environment operations")
		return false
	}
	if !s.agents.Supports(item.NodeID, "runtime_environment_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support runtime environment management")
		return false
	}
	return true
}

func (s *Server) queueEnvironmentRequest(w http.ResponseWriter, r *http.Request, item domain.RuntimeEnvironment, installationID, action string, payload map[string]any) {
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "runtime-environment", item.ID, action, map[string]any{
		"installation_id": installationID, "payload": payload,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.queueEnvironmentRequestWithFingerprint(w, r, item, installationID, action, payload, fingerprint)
}

func (s *Server) queueEnvironmentRequestWithFingerprint(w http.ResponseWriter, r *http.Request, item domain.RuntimeEnvironment, installationID, action string, payload map[string]any, fingerprint string) {
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueEnvironmentJob(r.Context(), item.ID, installationID, action, payload, principal.User.ID,
		r.Header.Get("Idempotency-Key"), fingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, action+".requested", "environment", item.ID, map[string]any{"job_id": job.ID, "game_installation_id": installationID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleInspectEnvironment(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsManage)
	if !ok || !s.requireEnvironmentAgent(w, item) {
		return
	}
	installations, err := s.store.ListGameInstallations(r.Context(), item.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	known := make([]map[string]any, 0, len(installations))
	for _, installation := range installations {
		known = append(known, map[string]any{"id": installation.ID, "path": installation.Path})
	}
	s.queueEnvironmentRequest(w, r, item, "", "environment.inspect", map[string]any{"installations": known})
}

func (s *Server) handleSteamCMDAction(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsManage)
	if !ok || !s.requireEnvironmentAgent(w, item) {
		return
	}
	var request struct {
		Action string `json:"action"`
		Path   string `json:"path,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Action = strings.ToLower(strings.TrimSpace(request.Action))
	allowed := map[string]string{"configure": "steamcmd.configure", "clear": "steamcmd.clear", "install": "steamcmd.install", "repair": "steamcmd.repair"}
	action := allowed[request.Action]
	if action == "" {
		writeError(w, http.StatusBadRequest, "invalid_action", "action must be configure, clear, install, or repair")
		return
	}
	payload := map[string]any{}
	if request.Action == "configure" {
		if strings.TrimSpace(request.Path) == "" {
			writeError(w, http.StatusBadRequest, "invalid_steamcmd_path", "path is required")
			return
		}
		payload["path"] = request.Path
	}
	s.queueEnvironmentRequest(w, r, item, "", action, payload)
}

func (s *Server) handleListGameInstallations(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsRead)
	if !ok {
		return
	}
	installations, err := s.store.ListGameInstallations(r.Context(), item.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": installations})
}

func (s *Server) handleCreateGameInstallation(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsManage)
	if !ok || !s.requireEnvironmentAgent(w, item) {
		return
	}
	var request struct {
		Path      string `json:"path"`
		Operation string `json:"operation"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Operation = strings.ToLower(strings.TrimSpace(request.Operation))
	if request.Operation != "install" && request.Operation != "import" {
		writeError(w, http.StatusBadRequest, "invalid_operation", "operation must be install or import")
		return
	}
	if !s.agents.Supports(item.NodeID, "absolute_game_paths") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support absolute game installation paths")
		return
	}
	if err := validateImportLocation(request.Path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_game_installation_path", err.Error())
		return
	}
	action := "game." + request.Operation
	principal := principalFromContext(r.Context())
	idempotencyKey := r.Header.Get("Idempotency-Key")
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "game-installation-create", item.ID, action, request)
	if err != nil {
		s.fail(w, err)
		return
	}
	if existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, fingerprint); lookupErr == nil {
		if existing.NodeID != item.NodeID || existing.InstallationID == "" || existing.Type != action {
			s.fail(w, ErrConflict)
			return
		}
		installation, installationErr := s.store.GetGameInstallation(r.Context(), existing.InstallationID)
		if installationErr != nil {
			s.fail(w, installationErr)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"game_installation": installation, "job_id": existing.ID, "idempotent_replay": true})
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.fail(w, lookupErr)
		return
	}
	installation, err := s.store.CreateGameInstallation(r.Context(), item.ID, request.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	payload := map[string]any{"path": installation.Path, "app_id": installation.AppID}
	job, reused, err := s.store.QueueEnvironmentJob(r.Context(), item.ID, installation.ID, action, payload, principal.User.ID,
		idempotencyKey, fingerprint)
	if err != nil {
		_ = s.store.MarkGameInstallationQueueFailed(r.Context(), installation.ID)
		s.fail(w, err)
		return
	}
	s.audit(r, "game_installation.created", "environment", item.ID, map[string]any{"game_installation_id": installation.ID, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"game_installation": installation, "job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) installationForRequest(w http.ResponseWriter, r *http.Request, item domain.RuntimeEnvironment) (domain.GameInstallation, bool) {
	installation, err := s.store.GetGameInstallation(r.Context(), chi.URLParam(r, "installationID"))
	if err != nil {
		s.fail(w, err)
		return domain.GameInstallation{}, false
	}
	if installation.EnvironmentID != item.ID {
		s.fail(w, ErrNotFound)
		return domain.GameInstallation{}, false
	}
	return installation, true
}

func gameInstallationActionAllowed(action string) bool {
	switch action {
	case "install", "import", "validate", "update":
		return true
	default:
		return false
	}
}

func (s *Server) handleGameInstallationAction(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsManage)
	if !ok || !s.requireEnvironmentAgent(w, item) {
		return
	}
	installation, ok := s.installationForRequest(w, r, item)
	if !ok {
		return
	}
	var request struct {
		Action string  `json:"action"`
		Path   *string `json:"path,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Action = strings.ToLower(strings.TrimSpace(request.Action))
	if !gameInstallationActionAllowed(request.Action) {
		writeError(w, http.StatusBadRequest, "invalid_action", "action must be install, import, validate, or update")
		return
	}
	if request.Path != nil && request.Action != "import" {
		writeError(w, http.StatusBadRequest, "invalid_game_installation_path", "path may only be supplied for import")
		return
	}
	if request.Path != nil {
		if !s.agents.Supports(item.NodeID, "absolute_game_paths") {
			writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support absolute game installation paths")
			return
		}
		if err := validateImportLocation(*request.Path); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_game_installation_path", err.Error())
			return
		}
	}
	if !s.agents.Supports(item.NodeID, "absolute_game_paths") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support this absolute game installation path")
		return
	}
	action := "game." + request.Action
	principal := principalFromContext(r.Context())
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "game-installation-action", installation.ID, action, request)
	if err != nil {
		s.fail(w, err)
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, fingerprint); lookupErr == nil {
		if existing.InstallationID != installation.ID || existing.Type != action {
			s.fail(w, ErrConflict)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": existing.ID, "idempotent_replay": true})
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.fail(w, lookupErr)
		return
	}
	if request.Path != nil {
		installation, err = s.store.UpdateGameInstallationLocation(r.Context(), installation.ID, *request.Path)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	payload := map[string]any{"path": installation.Path, "app_id": installation.AppID}
	if request.Action == "import" && installation.BoundInstanceID != "" {
		instance, instanceErr := s.store.GetInstance(r.Context(), installation.BoundInstanceID)
		if instanceErr != nil {
			s.fail(w, instanceErr)
			return
		}
		payload["instance"], payload["name"], payload["port"] = instance, instance.Name, instance.Port
	}
	s.queueEnvironmentRequestWithFingerprint(w, r, item, installation.ID, action, payload, fingerprint)
}

func (s *Server) handleDeleteGameInstallation(w http.ResponseWriter, r *http.Request) {
	item, ok := s.environmentForRequest(w, r, PermissionEnvironmentsManage)
	if !ok || !s.requireEnvironmentAgent(w, item) {
		return
	}
	if !s.agents.Supports(item.NodeID, "game_installation_delete_v1") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support game installation deletion")
		return
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Mode = strings.ToLower(strings.TrimSpace(request.Mode))
	if request.Mode != "record" && request.Mode != "files" {
		writeError(w, http.StatusBadRequest, "invalid_deletion_mode", "mode must be record or files")
		return
	}
	installationID := chi.URLParam(r, "installationID")
	fingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "game-installation-delete", installationID, request.Mode, request)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if existing, lookupErr := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, fingerprint); lookupErr == nil {
		if existing.NodeID != item.NodeID || existing.Type != "game.delete" || stringFromAny(existing.Payload["installation_id"]) != installationID {
			s.fail(w, ErrConflict)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": existing.ID, "idempotent_replay": true})
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.fail(w, lookupErr)
		return
	}
	installation, ok := s.installationForRequest(w, r, item)
	if !ok {
		return
	}
	payload := map[string]any{
		"installation_id": installation.ID,
		"path":            installation.Path,
		"mode":            request.Mode,
	}
	s.queueEnvironmentRequestWithFingerprint(w, r, item, installation.ID, "game.delete", payload, fingerprint)
}
