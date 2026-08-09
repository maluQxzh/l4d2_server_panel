package control

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Server) handleInstancePlayers(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.populateInstanceSecretState(r.Context(), &instance); err != nil {
		s.fail(w, err)
		return
	}
	latest, _ := s.store.LatestSuccessfulJob(r.Context(), instanceID, "rcon.players", map[string]any{})
	if rconPlayerRefreshAllowed(instance) {
		s.queueRefresh(r, instanceID, "rcon.players", map[string]any{})
	}
	items := make([]any, 0)
	if raw, ok := latest.Result["players"].([]any); ok {
		for _, entry := range raw {
			player, _ := entry.(map[string]any)
			item := map[string]any{"id": player["user_id"], "name": player["name"], "steam_id": player["steam_id"],
				"ping": player["ping"], "bot": player["bot"], "connected": player["connected"]}
			if connected, ok := player["connected"].(string); ok {
				if duration, valid := parseConnectedDuration(connected); valid {
					item["connected_at"] = time.Now().UTC().Add(-duration)
				}
			}
			items = append(items, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func rconPlayerRefreshAllowed(instance domain.Instance) bool {
	return instance.RCONConfigured && (instance.Status == domain.InstanceRunning || instance.Status == domain.InstanceDegraded)
}

func (s *Server) handleInstanceBans(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListPlayerBans(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func parseConnectedDuration(value string) (time.Duration, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	values := make([]int, len(parts))
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || (index > 0 && value > 59) {
			return 0, false
		}
		values[index] = value
	}
	if len(values) == 2 {
		return time.Duration(values[0])*time.Minute + time.Duration(values[1])*time.Second, true
	}
	return time.Duration(values[0])*time.Hour + time.Duration(values[1])*time.Minute + time.Duration(values[2])*time.Second, true
}

func (s *Server) handleInstanceFiles(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	if _, err := s.store.GetInstance(r.Context(), instanceID); err != nil {
		s.fail(w, err)
		return
	}
	relative := r.URL.Query().Get("path")
	payload := map[string]any{"relative_path": relative}
	latest, _ := s.store.LatestSuccessfulJob(r.Context(), instanceID, "list", payload)
	s.queueRefresh(r, instanceID, "list", payload)
	items := make([]any, 0)
	if raw, ok := latest.Result["files"].([]any); ok {
		for _, entry := range raw {
			file, _ := entry.(map[string]any)
			kind := "file"
			if isDir, _ := file["is_dir"].(bool); isDir {
				kind = "directory"
			}
			items = append(items, map[string]any{"name": file["name"], "path": file["path"], "type": kind,
				"size": file["size"], "modified_at": file["modified_at"]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) queueRefresh(r *http.Request, instanceID, jobType string, payload map[string]any) {
	principal := principalFromContext(r.Context())
	bucket := time.Now().Unix() / 10
	key := fmt.Sprintf("refresh:%s:%s:%v:%d", instanceID, jobType, payload, bucket)
	requestedBy := principal.User.ID
	if jobType == "rcon.players" {
		requestedBy = ""
	}
	job, _, err := s.store.QueueInstanceJob(r.Context(), instanceID, jobType, payload, requestedBy, key)
	if err == nil {
		s.dispatchSoon(job.NodeID)
	}
}

func (s *Server) handleInstancePackages(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListPackages(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleInstanceBackups(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListBackups(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{"id": item.ID, "name": item.Name, "size": item.Size, "platform": item.Platform,
			"checksum": item.SHA256, "created_at": item.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": result})
}

func (s *Server) handleInstanceSchedules(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListSchedules(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r, 100)
	queryLimit := min(500, limit*5)
	items, err := s.store.ListNotifications(r.Context(), queryLimit)
	if err != nil {
		s.fail(w, err)
		return
	}
	result := make([]map[string]any, 0, len(items))
	principal := principalFromContext(r.Context())
	for _, item := range items {
		if !s.principalAllowsNotification(r, principal, item) {
			continue
		}
		result = append(result, map[string]any{"id": item.ID, "level": item.Severity, "title": item.Title, "message": item.Message,
			"read": item.ReadAt != nil, "created_at": item.CreatedAt, "resource": item.Resource, "metadata": item.Metadata})
		if len(result) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": result})
}

func (s *Server) handleReadNotification(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "notificationID")
	item, err := s.store.Notification(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.principalAllowsNotification(r, principalFromContext(r.Context()), item) {
		writeError(w, http.StatusNotFound, "not_found", "notification not found")
		return
	}
	if err := s.store.ReadNotification(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "notification.read", "notification", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) principalAllowsNotification(r *http.Request, principal Principal, item domain.Notification) bool {
	if principal.GlobalAllows(PermissionNotificationsRead) {
		return true
	}
	if item.Resource == "" {
		return false
	}
	switch {
	case strings.HasPrefix(item.Type, "node."):
		return principal.AllowsNodePermission(item.Resource, PermissionNotificationsRead)
	case strings.HasPrefix(item.Type, "instance."), strings.HasPrefix(item.Type, "monitoring."), strings.HasPrefix(item.Type, "update."):
		instance, err := s.store.GetInstance(r.Context(), item.Resource)
		return err == nil && principal.AllowsInstancePermission(instance, PermissionNotificationsRead)
	case strings.HasPrefix(item.Type, "job."):
		job, err := s.store.GetJob(r.Context(), item.Resource)
		return err == nil && principal.AllowsJobPermission(job, PermissionNotificationsRead)
	default:
		return false
	}
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, allowed := requireAccountSecurity(w, r); !allowed {
		return
	}
	items, err := s.store.ListTeamUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func normalizePlayerCommand(action string, payload map[string]any) (string, error) {
	name, _ := payload["name"].(string)
	steamID, _ := payload["steam_id"].(string)
	reason, _ := payload["reason"].(string)
	if strings.ContainsAny(name, "\r\n;\"") || strings.ContainsAny(steamID, "\r\n;\"") {
		return "", errors.New("invalid player identifier")
	}
	if len([]rune(reason)) > 200 || strings.ContainsAny(reason, "\x00\r\n") {
		return "", errors.New("ban reason must contain at most 200 characters on one line")
	}
	switch action {
	case "kick_player":
		if name == "" {
			return "", errors.New("player name is required")
		}
		return `kick "` + name + `"`, nil
	case "ban_player":
		if !steamIdentityPattern.MatchString(steamID) {
			return "", errors.New("Steam ID is required")
		}
		duration := intFromAny(payload["duration_minutes"])
		if duration < 0 || duration > 525600 {
			return "", errors.New("ban duration must be between 0 and 525600 minutes")
		}
		return fmt.Sprintf("banid %d %s kick; writeid", duration, steamID), nil
	default:
		return "", errors.New("unsupported player action")
	}
}

var steamIdentityPattern = regexp.MustCompile(`^(?:STEAM_[0-5]:[01]:[0-9]+|\[U:1:[0-9]+\]|[0-9]{5,20})$`)

func safeBroadcastCommand(message string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" || len([]rune(message)) > 200 {
		return "", errors.New("broadcast message must contain 1 to 200 characters")
	}
	if strings.ContainsAny(message, "\x00\r\n;\"\\") {
		return "", errors.New("broadcast message contains unsafe characters")
	}
	return `say "` + message + `"`, nil
}

func packageNameFromSource(source string) string {
	if parsed, err := url.Parse(source); err == nil && parsed.Scheme != "" {
		source = parsed.Path
	}
	name := filepath.Base(strings.TrimRight(source, "/"))
	if name == "." || name == "/" || name == "" {
		return "package"
	}
	return name
}

func validatePackageUpload(payload map[string]any) error {
	encoded := stringFromAny(payload["content_base64"])
	if encoded == "" {
		return nil
	}
	if stringFromAny(payload["source"]) != "" {
		return errors.New("provide either source or content_base64, not both")
	}
	name := strings.TrimSpace(stringFromAny(payload["name"]))
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "\x00/\\") {
		return errors.New("uploaded package name is invalid")
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > 8<<20 {
		return errors.New("uploaded package exceeds 8 MiB")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > 8<<20 {
		return errors.New("uploaded package is not valid Base64 or exceeds 8 MiB")
	}
	return nil
}
