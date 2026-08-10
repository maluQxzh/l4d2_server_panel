package control

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "L4D2 Panel API", "version": "1.0.0"},
		"paths": map[string]any{
			"/api/v1/auth/bootstrap":            map[string]any{"get": map[string]any{"summary": "Check first-run setup status"}, "post": map[string]any{"summary": "Create the first owner"}},
			"/api/v1/auth/login":                map[string]any{"post": map[string]any{"summary": "Create a browser session"}},
			"/api/v1/auth/activate":             map[string]any{"post": map[string]any{"summary": "Complete first-login account activation"}},
			"/api/v1/system/updates":            map[string]any{"get": map[string]any{"summary": "List center updates"}, "post": map[string]any{"summary": "Upload and start a center update"}},
			"/api/v1/system/updates/{jobID}":    map[string]any{"get": map[string]any{"summary": "Get a center update"}},
			"/api/v1/nodes":                     map[string]any{"get": map[string]any{"summary": "List nodes"}},
			"/api/v1/nodes/{nodeID}/quarantine": map[string]any{"get": map[string]any{"summary": "List retained quarantine entries"}},
			"/api/v1/nodes/{nodeID}/quarantine/{entryID}/purge-confirmations": map[string]any{"post": map[string]any{"summary": "Create a one-time quarantine purge confirmation"}},
			"/api/v1/nodes/{nodeID}/quarantine/{entryID}/purge":               map[string]any{"post": map[string]any{"summary": "Queue a confirmed quarantine purge"}},
			"/api/v1/instances":                                                       map[string]any{"get": map[string]any{"summary": "List instances"}, "post": map[string]any{"summary": "Create an instance"}},
			"/api/v1/instances/{instanceID}/backup-uploads":                           map[string]any{"post": map[string]any{"summary": "Create a resumable backup upload"}},
			"/api/v1/instances/{instanceID}/backup-uploads/{uploadID}":                map[string]any{"get": map[string]any{"summary": "Get a backup upload"}, "delete": map[string]any{"summary": "Abandon a backup upload and queue staging cleanup"}},
			"/api/v1/instances/{instanceID}/backup-uploads/{uploadID}/chunks":         map[string]any{"post": map[string]any{"summary": "Queue one verified backup upload chunk"}},
			"/api/v1/instances/{instanceID}/backup-uploads/{uploadID}/chunks/{jobID}": map[string]any{"get": map[string]any{"summary": "Get a backup upload chunk status"}},
			"/api/v1/instances/{instanceID}/backup-uploads/{uploadID}/complete":       map[string]any{"post": map[string]any{"summary": "Complete an upload and queue restore"}},
			"/api/v1/instances/{instanceID}/plugin-archives/inspections":              map[string]any{"post": map[string]any{"summary": "Inspect a local ZIP, 7Z, or RAR plugin archive"}},
			"/api/v1/instances/{instanceID}/plugin-archives/inspections/{jobID}":      map[string]any{"get": map[string]any{"summary": "Get a plugin archive inspection"}},
			"/api/v1/jobs":                                                        map[string]any{"get": map[string]any{"summary": "List jobs"}},
			"/api/v1/metrics":                                                     map[string]any{"get": map[string]any{"summary": "Query raw, 1m, or 1h metrics"}},
			"/api/v1/users":                                                       map[string]any{"get": map[string]any{"summary": "List administrator team"}, "post": map[string]any{"summary": "Create a team member"}},
			"/api/v1/users/{userID}":                                              map[string]any{"patch": map[string]any{"summary": "Update, disable, or replace role bindings"}},
			"/api/v1/users/{userID}/role-bindings":                                map[string]any{"get": map[string]any{"summary": "List role bindings"}},
			"/api/v1/users/{userID}/activation":                                   map[string]any{"post": map[string]any{"summary": "Reissue a pending user's one-time password"}},
			"/api/v1/notification-channels":                                       map[string]any{"get": map[string]any{"summary": "List notification channels"}, "post": map[string]any{"summary": "Create a notification channel"}},
			"/api/v1/notification-deliveries":                                     map[string]any{"get": map[string]any{"summary": "List notification deliveries"}},
			"/api/v1/instances/{instanceID}/logs/search":                          map[string]any{"post": map[string]any{"summary": "Queue a bounded instance log search"}},
			"/api/v1/instances/{instanceID}/logs/search/{jobID}":                  map[string]any{"get": map[string]any{"summary": "Consume a completed log search"}},
			"/api/v1/instances/{instanceID}/logs/downloads":                       map[string]any{"post": map[string]any{"summary": "Queue a bounded log download chunk"}},
			"/api/v1/instances/{instanceID}/logs/downloads/{jobID}":               map[string]any{"get": map[string]any{"summary": "Consume a completed log chunk"}},
			"/api/v1/instances/{instanceID}/backups/{backupID}/downloads":         map[string]any{"post": map[string]any{"summary": "Queue a verified backup download chunk"}},
			"/api/v1/instances/{instanceID}/backups/{backupID}/downloads/{jobID}": map[string]any{"get": map[string]any{"summary": "Consume a completed backup chunk"}},
			"/api/v1/public/servers":                                              map[string]any{"get": map[string]any{"summary": "List public servers"}},
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(doc)
}

func (s *Server) handleOpenAPIYAML(w http.ResponseWriter, _ *http.Request) {
	candidates := []string{"docs/openapi.yaml"}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "docs", "openapi.yaml"), filepath.Join(filepath.Dir(executable), "openapi.yaml"))
	}
	for _, candidate := range candidates {
		if body, err := os.ReadFile(candidate); err == nil {
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			_, _ = w.Write(body)
			return
		}
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write([]byte("openapi: 3.1.0\ninfo:\n  title: L4D2 Panel API\n  version: 1.0.0\npaths: {}\n"))
}
