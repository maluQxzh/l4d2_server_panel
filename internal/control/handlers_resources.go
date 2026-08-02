package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	var request EnrollRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	node, credential, err := s.store.ConsumeEnrollmentToken(r.Context(), request, remoteIP(r))
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "invalid_enrollment_token", "enrollment token is invalid, expired, or already used")
			return
		}
		s.fail(w, err)
		return
	}
	_ = s.store.WriteAudit(r.Context(), domain.AuditEvent{ActorName: "agent-enrollment", Action: "node.enrolled", ResourceType: "node",
		ResourceID: node.ID, RemoteIP: remoteIP(r), Metadata: map[string]any{"name": node.Name, "os": node.OS, "arch": node.Arch}})
	_, _ = s.events.Publish(r.Context(), Event{Type: "node.enrolled", ResourceType: "node", ResourceID: node.ID, Data: map[string]any{"node": node}}, true)
	writeJSON(w, http.StatusCreated, map[string]any{"node_id": node.ID, "credential": credential})
}

func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	credential := bearerToken(r)
	if credential == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "agent credential is required")
		return
	}
	node, err := s.store.NodeByCredential(r.Context(), credential)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "agent credential is invalid or revoked")
		return
	}
	s.agents.Serve(node, remoteIP(r), w, r)
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListNodes(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if principal.HasResourceRestriction() {
		filtered := items[:0]
		for _, item := range items {
			if principal.AllowsNodePermission(item.ID, PermissionRead) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	node, err := s.store.GetNode(r.Context(), chi.URLParam(r, "nodeID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Label            string `json:"label,omitempty"`
		Name             string `json:"name,omitempty"`
		ExpiresInMinutes int    `json:"expires_in_minutes"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.ExpiresInMinutes == 0 {
		request.ExpiresInMinutes = 30
	}
	if request.Label == "" {
		request.Label = request.Name
	}
	principal := principalFromContext(r.Context())
	token, expires, err := s.store.CreateEnrollmentToken(r.Context(), request.Label, principal.User.ID, time.Duration(request.ExpiresInMinutes)*time.Minute)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_enrollment_token", err.Error())
		return
	}
	s.audit(r, "node.enrollment_token.created", "enrollment_token", "", map[string]any{"label": request.Label, "expires_at": expires})
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "expires_at": expires})
}

func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name          string  `json:"name"`
		PublicAddress *string `json:"public_address,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	node, err := s.store.UpdateNode(r.Context(), chi.URLParam(r, "nodeID"), request.Name, request.PublicAddress)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusBadRequest, "invalid_node", err.Error())
			return
		}
		s.fail(w, err)
		return
	}
	s.audit(r, "node.updated", "node", node.ID, map[string]any{"name": node.Name, "public_address": node.PublicAddress})
	_, _ = s.events.Publish(r.Context(), Event{Type: "node.updated", ResourceType: "node", ResourceID: node.ID, Data: map[string]any{"node": node}}, true)
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "nodeID")
	if err := s.store.RevokeNode(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.agents.Disconnect(id)
	s.audit(r, "node.revoked", "node", id, nil)
	_, _ = s.events.Publish(r.Context(), Event{Type: "node.revoked", ResourceType: "node", ResourceID: id, Data: map[string]any{"node_id": id}}, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRotateNodeCredential(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "nodeID")
	credential, err := s.store.RotateNodeCredential(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.agents.Disconnect(id)
	_ = s.store.SetNodeOffline(r.Context(), id)
	s.audit(r, "node.credential.rotated", "node", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"credential": credential})
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListInstances(r.Context(), r.URL.Query().Get("node_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	if principal.HasResourceRestriction() {
		filtered := items[:0]
		for _, item := range items {
			if principal.AllowsInstancePermission(item, PermissionRead) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	instance, err := s.store.GetInstance(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance})
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NodeID         string                `json:"node_id"`
		InstallationID string                `json:"game_installation_id,omitempty"`
		Name           string                `json:"name"`
		Path           string                `json:"path,omitempty"`
		RootIndex      *int                  `json:"root_index,omitempty"`
		Executable     string                `json:"executable,omitempty"`
		Host           string                `json:"host"`
		Port           int                   `json:"port"`
		TVPort         int                   `json:"tv_port"`
		Operation      string                `json:"operation,omitempty"`
		Config         domain.InstanceConfig `json:"config"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Operation != "" && request.Operation != "import" {
		writeError(w, http.StatusBadRequest, "invalid_operation", "instances only bind or import an existing game installation")
		return
	}
	if request.Operation == "" {
		request.Operation = "import"
	}
	if err := validateStructuredConfig(request.Config); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	if request.Operation == "import" && request.InstallationID == "" {
		if err := validateImportLocation(request.Path, request.RootIndex); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_import_path", err.Error())
			return
		}
		if request.Executable != "" && request.Executable != "srcds.exe" && request.Executable != "srcds_run" && request.Executable != "srcds_linux" {
			writeError(w, http.StatusBadRequest, "invalid_import_executable", "executable must be srcds.exe, srcds_run, or srcds_linux")
			return
		}
	}
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(request.NodeID, PermissionManage) {
		writeTokenScopeDenied(w)
		return
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-create", request.NodeID, "create", request)
	if err != nil {
		s.fail(w, err)
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if existing, err := s.store.JobByIdempotencyKeyMatching(r.Context(), principal.User.ID, idempotencyKey, requestFingerprint); err == nil {
		if !principal.AllowsJobPermission(existing, PermissionManage) {
			writeTokenScopeDenied(w)
			return
		}
		response := map[string]any{"job_id": existing.ID, "idempotent_replay": true}
		if instance, instanceErr := s.store.GetInstance(r.Context(), existing.InstanceID); instanceErr == nil {
			response["instance"] = instance
		}
		writeJSON(w, http.StatusAccepted, response)
		return
	} else if !errors.Is(err, ErrNotFound) {
		s.fail(w, err)
		return
	}
	serverPassword := request.Config.ServerPassword
	provisionalInstallationID := ""
	removeProvisionalInstallation := func() { _ = s.store.RemovePendingGameInstallation(r.Context(), provisionalInstallationID) }
	rconPassword, err := randomToken(24)
	if err != nil {
		s.fail(w, err)
		return
	}
	request.Config.ServerPassword = ""
	if request.InstallationID != "" {
		installation, lookupErr := s.store.GetGameInstallation(r.Context(), request.InstallationID)
		if lookupErr != nil {
			s.fail(w, lookupErr)
			return
		}
		if installation.NodeID != request.NodeID || installation.Status != "ready" || installation.BoundInstanceID != "" {
			s.fail(w, ErrConflict)
			return
		}
		request.Operation, request.Path = "import", installation.RelativePath
		rootIndex := installation.RootIndex
		request.RootIndex = &rootIndex
	} else if request.Operation == "import" {
		rootIndex := 0
		if request.RootIndex != nil {
			rootIndex = *request.RootIndex
		}
		installation, createErr := s.store.CreateGameInstallation(r.Context(), "environment-"+request.NodeID, rootIndex, request.Path)
		if createErr != nil {
			s.fail(w, createErr)
			return
		}
		request.InstallationID = installation.ID
		provisionalInstallationID = installation.ID
	}
	instance, err := s.store.CreateInstance(r.Context(), CreateInstanceInput{NodeID: request.NodeID, Name: request.Name,
		InstallationID: request.InstallationID, Path: request.Path, Host: request.Host, Port: request.Port, TVPort: request.TVPort, Config: request.Config})
	if err != nil {
		removeProvisionalInstallation()
		if errors.Is(err, ErrConflict) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_instance", err.Error())
		}
		return
	}
	encryptedRCON, err := encryptSecret(s.config.MasterKey, rconPassword)
	if err != nil {
		_ = s.store.RemoveInstanceRecord(r.Context(), instance.ID)
		removeProvisionalInstallation()
		s.fail(w, err)
		return
	}
	encryptedServerPassword := ""
	if serverPassword != "" {
		encryptedServerPassword, err = encryptSecret(s.config.MasterKey, serverPassword)
		if err != nil {
			_ = s.store.RemoveInstanceRecord(r.Context(), instance.ID)
			removeProvisionalInstallation()
			s.fail(w, err)
			return
		}
	}
	if err := s.store.SaveInstanceSecrets(r.Context(), instance.ID, encryptedRCON, encryptedServerPassword); err != nil {
		_ = s.store.RemoveInstanceRecord(r.Context(), instance.ID)
		removeProvisionalInstallation()
		s.fail(w, err)
		return
	}
	if _, err := s.store.CreateSchedule(r.Context(), instance.ID, "Daily automatic backup", "0 4 * * *", s.config.Timezone,
		"backup", map[string]any{"kind": "daily", "retention_daily": 7, "retention_weekly": 4}, true); err != nil {
		_ = s.store.RemoveInstanceRecord(r.Context(), instance.ID)
		removeProvisionalInstallation()
		s.fail(w, err)
		return
	}
	jobType, operation := "game.import", "import"
	jobPayload := map[string]any{"operation": operation, "instance": instance, "path": request.Path, "name": instance.Name, "port": instance.Port}
	if request.Executable != "" {
		jobPayload["executable"] = request.Executable
	}
	if request.RootIndex != nil {
		jobPayload["root_index"] = *request.RootIndex
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instance.ID, jobType, jobPayload, principal.User.ID,
		idempotencyKey, requestFingerprint)
	if err != nil {
		_ = s.store.RemoveInstanceRecord(r.Context(), instance.ID)
		removeProvisionalInstallation()
		s.fail(w, err)
		return
	}
	if reused && job.InstanceID != instance.ID {
		// A concurrent request can create its provisional instance after both
		// preflight lookups miss. The unique key selects the first job; remove
		// this request's unused record and return the selected instance.
		_ = s.store.RemoveInstanceRecord(r.Context(), instance.ID)
		removeProvisionalInstallation()
		original, lookupErr := s.store.GetInstance(r.Context(), job.InstanceID)
		if lookupErr != nil {
			s.fail(w, lookupErr)
			return
		}
		s.dispatchSoon(job.NodeID)
		writeJSON(w, http.StatusAccepted, map[string]any{"instance": original, "job_id": job.ID, "idempotent_replay": true})
		return
	}
	s.audit(r, "instance.created", "instance", instance.ID, map[string]any{"node_id": instance.NodeID, "port": instance.Port})
	_, _ = s.events.Publish(r.Context(), Event{Type: "instance.created", ResourceType: "instance", ResourceID: instance.ID, Data: map[string]any{"instance": instance, "job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"instance": instance, "job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handlePatchInstance(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name           string                 `json:"name"`
		Config         *domain.InstanceConfig `json:"config,omitempty"`
		ServerPassword *string                `json:"server_password,omitempty"`
		Version        int                    `json:"version"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	instanceID := chi.URLParam(r, "instanceID")
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-config", instanceID, "configure", request)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	var serverPassword *string
	if request.ServerPassword != nil {
		serverPassword = request.ServerPassword
	} else if request.Config != nil && request.Config.ServerPassword != "" {
		serverPassword = &request.Config.ServerPassword
	}
	if serverPassword != nil {
		if err := validateServerPassword(*serverPassword); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
			return
		}
	}
	if request.Config != nil {
		if err := validateStructuredConfig(*request.Config); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
			return
		}
		request.Config.ServerPassword = ""
	}
	var encryptedServerPassword *string
	if serverPassword != nil {
		encrypted := ""
		var err error
		if *serverPassword != "" {
			encrypted, err = encryptSecret(s.config.MasterKey, *serverPassword)
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		encryptedServerPassword = &encrypted
	}
	instance, job, reused, err := s.store.UpdateInstanceAndQueueWithFingerprint(r.Context(), instanceID, request.Name, request.Config,
		request.Version, principal.User.ID, r.Header.Get("Idempotency-Key"), encryptedServerPassword, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principal.AllowsJob(job) {
		writeTokenScopeDenied(w)
		return
	}
	s.audit(r, "instance.updated", "instance", instance.ID, map[string]any{"version": instance.Version, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "instance.updated", ResourceType: "instance", ResourceID: instance.ID, Data: map[string]any{"instance": instance, "job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusOK, map[string]any{"instance": instance, "job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "detach"
	}
	if mode != "detach" && mode != "quarantine" {
		writeError(w, http.StatusBadRequest, "invalid_delete_mode", "mode must be detach or quarantine")
		return
	}
	if mode == "quarantine" {
		instance, err := s.store.GetInstance(r.Context(), chi.URLParam(r, "instanceID"))
		if err != nil {
			s.fail(w, err)
			return
		}
		if !s.agents.Connected(instance.NodeID) {
			writeError(w, http.StatusConflict, "node_offline", "node must be online to verify safe quarantine retention support")
			return
		}
		if !s.agents.Supports(instance.NodeID, "purge_quarantine") {
			writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support confirmed quarantine purge")
			return
		}
	}
	principal := principalFromContext(r.Context())
	instanceID := chi.URLParam(r, "instanceID")
	payload := map[string]any{"mode": mode}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-delete", instanceID, "delete", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "delete", payload, principal.User.ID,
		r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principal.AllowsJob(job) {
		writeTokenScopeDenied(w)
		return
	}
	s.audit(r, "instance.delete.requested", "instance", job.InstanceID, map[string]any{"mode": mode, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleInstanceAction(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload,omitempty"`
	}
	if !decodeJSONWithLimit(w, r, &request, 12<<20) {
		return
	}
	principal := principalFromContext(r.Context())
	instanceID := chi.URLParam(r, "instanceID")
	if request.Payload == nil {
		request.Payload = make(map[string]any)
	}
	if request.Type == "delete" || request.Type == "detach" {
		writeError(w, http.StatusBadRequest, "dedicated_delete_required", "delete and detach must use DELETE /api/v1/instances/{instanceId}?mode=quarantine|detach")
		return
	}
	if request.Type == "rollback_package" {
		writeError(w, http.StatusBadRequest, "invalid_action", "package rollback must use the version-scoped endpoint")
		return
	}
	if !genericInstanceActionAllowed(request.Type) {
		writeError(w, http.StatusBadRequest, "invalid_action", "unsupported instance action")
		return
	}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-action", instanceID, request.Type, request.Payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	requiredPermission, _ := genericInstanceActionPermission(request.Type)
	if !principal.Allows(requiredPermission) {
		writeError(w, http.StatusForbidden, "forbidden", "this action requires administrator permission")
		return
	}
	if request.Type == "create_schedule" {
		scheduledAction, _ := request.Payload["action"].(string)
		scheduledPermission, supported := genericInstanceActionPermission(scheduledAction)
		if !supported {
			writeError(w, http.StatusBadRequest, "invalid_schedule", "unsupported scheduled action")
			return
		}
		if !principal.Allows(scheduledPermission) {
			writeError(w, http.StatusForbidden, "forbidden", "this scheduled action requires administrator permission")
			return
		}
		if !scheduledActionAllowed(scheduledAction) {
			writeError(w, http.StatusBadRequest, "invalid_schedule", "unsupported scheduled action")
			return
		}
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principal.AllowsInstancePermission(instance, requiredPermission) {
		writeError(w, http.StatusForbidden, "forbidden", "this action is outside the permitted instance scope")
		return
	}
	if instance.Status == domain.InstanceExternal && request.Type == "create_schedule" {
		scheduledAction, _ := request.Payload["action"].(string)
		scheduledPayload, _ := request.Payload["payload"].(map[string]any)
		if scheduledAction == "restart" || !externalInstanceJobAllowed(scheduledAction, scheduledPayload) {
			writeError(w, http.StatusConflict, "external_instance_read_only", "external instances only allow read-only schedules until an explicit controlled restart")
			return
		}
	}
	if request.Type == "create_schedule" {
		name, _ := request.Payload["name"].(string)
		expression, _ := request.Payload["cron"].(string)
		action, _ := request.Payload["action"].(string)
		timezone, _ := request.Payload["timezone"].(string)
		if timezone == "" {
			timezone = s.config.Timezone
		}
		enabled, ok := request.Payload["enabled"].(bool)
		if !ok {
			enabled = true
		}
		if _, ok := genericInstanceActionPermission(action); !ok || !scheduledActionAllowed(action) {
			writeError(w, http.StatusBadRequest, "invalid_schedule", "unsupported scheduled action")
			return
		}
		scheduledPermission, _ := genericInstanceActionPermission(action)
		if !principal.AllowsInstancePermission(instance, scheduledPermission) {
			writeError(w, http.StatusForbidden, "forbidden", "this scheduled action requires administrator permission")
			return
		}
		scheduledPayload := make(map[string]any)
		if nested, ok := request.Payload["payload"].(map[string]any); ok {
			for key, value := range nested {
				scheduledPayload[key] = value
			}
		}
		if isLifecycleAction(action) {
			var sanitizeErr error
			scheduledPayload, sanitizeErr = canonicalLifecyclePayload(action, scheduledPayload, instance)
			if sanitizeErr != nil {
				writeError(w, http.StatusBadRequest, "invalid_schedule_payload", sanitizeErr.Error())
				return
			}
			delete(scheduledPayload, "instance")
		}
		if action == "backup" {
			if _, ok := scheduledPayload["kind"]; !ok {
				scheduledPayload["kind"] = "daily"
			}
		}
		if action == "broadcast" {
			message, _ := request.Payload["message"].(string)
			if message == "" {
				message = name
			}
			if _, err := safeBroadcastCommand(message); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_broadcast", err.Error())
				return
			}
			scheduledPayload = map[string]any{"message": message}
		}
		scheduledPermission, _ = genericInstanceActionPermission(action)
		if !principal.AllowsInstancePermission(instance, scheduledPermission) {
			writeError(w, http.StatusForbidden, "forbidden", "this scheduled action requires administrator permission")
			return
		}
		schedule, err := s.store.CreateSchedule(r.Context(), instanceID, name, expression, timezone, action, scheduledPayload, enabled)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
			return
		}
		s.audit(r, "schedule.created", "schedule", schedule.ID, map[string]any{"instance_id": instanceID})
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": schedule.ID, "schedule": schedule})
		return
	}
	if request.Type == "toggle_package" {
		packageID, _ := request.Payload["package_id"].(string)
		pkg, path, err := s.store.PackageForToggle(r.Context(), instanceID, packageID)
		if err != nil {
			s.fail(w, err)
			return
		}
		request.Payload["path"] = path
		request.Payload["kind"] = pkg.Kind
	}
	originalType := request.Type
	if isLifecycleAction(request.Type) {
		request.Payload, err = canonicalLifecyclePayload(request.Type, request.Payload, instance)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_action_payload", err.Error())
			return
		}
	}
	if request.Type == "backup" {
		if _, ok := request.Payload["kind"]; !ok {
			request.Payload["kind"] = "manual"
		}
	}
	switch request.Type {
	case "kick_player", "ban_player":
		_, err := normalizePlayerCommand(request.Type, request.Payload)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_player_action", err.Error())
			return
		}
	case "unban_player":
		steamID, _ := request.Payload["steam_id"].(string)
		if !steamIdentityPattern.MatchString(steamID) {
			writeError(w, http.StatusBadRequest, "invalid_player_action", "Steam ID is invalid")
			return
		}
	case "change_map":
		mapName, _ := request.Payload["map"].(string)
		if strings.ContainsAny(mapName, "\r\n;\" ") || mapName == "" {
			writeError(w, http.StatusBadRequest, "invalid_map", "map name is invalid")
			return
		}
	case "broadcast":
		message, _ := request.Payload["message"].(string)
		command, err := safeBroadcastCommand(message)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_broadcast", err.Error())
			return
		}
		request.Type, request.Payload = "rcon", map[string]any{"command": command}
	case "install_package":
		if err := validatePackageUpload(request.Payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_package_upload", err.Error())
			return
		}
		request.Type = "install_mod"
		request.Payload["risk_confirmed"], _ = request.Payload["acknowledge_native_risk"].(bool)
		if _, ok := request.Payload["name"]; !ok {
			request.Payload["name"] = packageNameFromSource(stringFromAny(request.Payload["source"]))
		}
	case "restore":
		if backupID, _ := request.Payload["backup_id"].(string); backupID != "" {
			path, err := s.store.BackupPath(r.Context(), instanceID, backupID)
			if err != nil {
				s.fail(w, err)
				return
			}
			request.Payload["archive"] = path
		}
		request.Payload["instance"] = instance
	}
	if request.Type == "restart" && instance.Status == domain.InstanceExternal {
		// The Agent needs the current structured configuration when it stops
		// the verified external PID and starts the server under process-tree
		// supervision for the first time.
		request.Payload["instance"] = instance
	}
	requiredPermission, _ = InstanceActionPermission(request.Type)
	if !principal.AllowsInstancePermission(instance, requiredPermission) {
		writeError(w, http.StatusForbidden, "forbidden", "this action requires administrator permission")
		return
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, request.Type, request.Payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_secret_payload", err.Error())
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, request.Type, request.Payload,
		principal.User.ID, r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			s.fail(w, err)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_action", err.Error())
		}
		return
	}
	if !principal.AllowsJob(job) {
		writeTokenScopeDenied(w)
		return
	}
	s.audit(r, "instance.action.requested", "instance", job.InstanceID, map[string]any{"type": originalType, "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func validateStructuredConfig(config domain.InstanceConfig) error {
	return validateInstanceConfig(config)
}

func isLifecycleAction(action string) bool {
	switch action {
	case "start", "stop", "restart", "update", "validate", "reinstall":
		return true
	default:
		return false
	}
}

func canonicalLifecyclePayload(action string, supplied map[string]any, instance domain.Instance) (map[string]any, error) {
	allowed := map[string]bool{}
	switch action {
	case "start":
		allowed["health_timeout_seconds"] = true
	case "stop":
		allowed["timeout_seconds"], allowed["maintenance"], allowed["warning_seconds"] = true, true, true
	case "restart", "update", "validate", "reinstall":
		allowed["timeout_seconds"], allowed["maintenance"], allowed["warning_seconds"], allowed["health_timeout_seconds"] = true, true, true, true
		if action == "restart" && instance.Status == domain.InstanceExternal {
			allowed["existing_rcon_password"] = true
		}
	default:
		return nil, errors.New("unsupported lifecycle action")
	}
	result := make(map[string]any, len(allowed)+1)
	for key, value := range supplied {
		if !allowed[key] {
			return nil, fmt.Errorf("field %q cannot be supplied for %s", key, action)
		}
		result[key] = value
	}
	if action != "stop" {
		result["instance"] = instance
	}
	return result, nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	nodeID, instanceID := r.URL.Query().Get("node_id"), r.URL.Query().Get("instance_id")
	if nodeID != "" && instanceID == "" && !principal.AllowsNodePermission(nodeID, PermissionRead) {
		writeTokenScopeDenied(w)
		return
	}
	if instanceID != "" {
		instance, lookupErr := s.store.GetInstance(r.Context(), instanceID)
		if lookupErr != nil {
			s.fail(w, lookupErr)
			return
		}
		if !principal.AllowsInstancePermission(instance, PermissionRead) || (nodeID != "" && instance.NodeID != nodeID) {
			writeTokenScopeDenied(w)
			return
		}
	}
	limit := parseLimit(r, 100)
	queryLimit := limit
	if principal.HasResourceRestriction() {
		queryLimit = limit * 5
		if queryLimit > 500 {
			queryLimit = 500
		}
	}
	items, err := s.store.ListJobs(r.Context(), nodeID, instanceID, queryLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	filtered := make([]domain.Job, 0, min(limit, len(items)))
	for _, item := range items {
		if principal.AllowsJobPermission(item, PermissionRead) {
			filtered = append(filtered, redactJobForPrincipal(item, principal))
			if len(filtered) == limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": filtered})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	job = redactJobForPrincipal(job, principalFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func redactJob(job domain.Job, role domain.Role) domain.Job {
	if !RoleAllows(role, PermissionOperate) {
		job.Payload = nil
		job.Result = nil
		return job
	}
	job.Payload = cloneMap(job.Payload)
	job.Result = cloneMap(job.Result)
	stripJobFields(job.Type, job.Payload, job.Result)
	return job
}

func redactJobForPrincipal(job domain.Job, principal Principal) domain.Job {
	if !principal.AllowsJobPermission(job, PermissionOperate) {
		job.Payload = nil
		job.Result = nil
		return job
	}
	job.Payload = cloneMap(job.Payload)
	job.Result = cloneMap(job.Result)
	stripJobFields(job.Type, job.Payload, job.Result)
	return job
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	job, previousStatus, err := s.store.CancelJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	cancelPending, _ := job.Result["cancel_pending"].(bool)
	cancelDelivered := false
	if cancelPending {
		cancelDelivered, err = s.agents.Cancel(job.NodeID, job.ID)
		if err != nil {
			s.logger.Warn("deliver job cancellation", "job_id", job.ID, "node_id", job.NodeID, "error", err)
			_ = s.store.CreateNotificationOnce(r.Context(), "warning", "job.cancel.delivery_failed", "Task cancellation not delivered",
				"The node disconnected before the cancellation request could be delivered", job.ID,
				map[string]any{"job_id": job.ID, "node_id": job.NodeID}, time.Hour)
		}
	}
	s.agents.forgetSent(job.NodeID, job.ID)
	s.audit(r, "job.canceled", "job", job.ID, map[string]any{"previous_status": previousStatus, "cancel_delivered": cancelDelivered})
	publicJob := redactJobForPrincipal(job, principalFromContext(r.Context()))
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.updated", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": publicJob}}, true)
	writeJSON(w, http.StatusOK, map[string]any{"job": publicJob, "cancel_delivered": cancelDelivered})
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	resourceID := r.URL.Query().Get("resource_id")
	if principal.HasResourceRestriction() {
		if resourceID != "" && !s.tokenAllowsAuditResource(r.Context(), principal, resourceID) {
			writeTokenScopeDenied(w)
			return
		}
	}
	if resourceID == "" && !principal.GlobalAllows(PermissionManage) && !principal.HasResourceRestriction() {
		writeError(w, http.StatusForbidden, "forbidden", "global audit access requires administrator permission")
		return
	}
	limit := parseLimit(r, 100)
	queryLimit := limit
	if resourceID == "" && principal.HasResourceRestriction() {
		queryLimit = limit * 5
		if queryLimit > 500 {
			queryLimit = 500
		}
	}
	items, err := s.store.ListAudit(r.Context(), resourceID, queryLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	if principal.HasResourceRestriction() {
		filtered := make([]domain.AuditEvent, 0, min(limit, len(items)))
		for _, item := range items {
			if s.auditEventAllowed(r.Context(), principal, item) {
				filtered = append(filtered, item)
				if len(filtered) == limit {
					break
				}
			}
		}
		items = filtered
	}
	if !principal.GlobalAllows(PermissionManage) {
		for i := range items {
			items[i].RemoteIP = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handlePublicServers(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.PublicServers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) dispatchSoon(nodeID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.agents.Dispatch(ctx, nodeID)
	}()
}

func parseInt(value string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return n
}

func validateImportLocation(relativePath string, rootIndex *int) error {
	if rootIndex == nil {
		return errors.New("root_index is required for import")
	}
	if *rootIndex < 0 {
		return errors.New("root_index must not be negative")
	}
	normalized := strings.ReplaceAll(strings.TrimSpace(relativePath), `\`, "/")
	if normalized == "" {
		return errors.New("path is required for import")
	}
	if strings.HasPrefix(normalized, "/") || (len(normalized) >= 2 && normalized[1] == ':') {
		return errors.New("path must be relative to the selected instance root")
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return errors.New("path must stay within the selected instance root")
		}
	}
	return nil
}
