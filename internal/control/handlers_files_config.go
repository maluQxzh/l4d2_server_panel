package control

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	pathpkg "path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type configFieldChange struct {
	Before any `json:"before,omitempty"`
	After  any `json:"after,omitempty"`
}

type configVersionResponse struct {
	ConfigVersion
	Diff map[string]configFieldChange `json:"diff"`
}

func (s *Server) handleConfigVersions(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListConfigVersions(r.Context(), chi.URLParam(r, "instanceID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	response := make([]configVersionResponse, 0, len(items))
	for index, item := range items {
		entry := configVersionResponse{ConfigVersion: item, Diff: map[string]configFieldChange{}}
		if index+1 < len(items) {
			entry.Diff = diffInstanceConfig(items[index+1].Config, item.Config)
		}
		response = append(response, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": response})
}

func (s *Server) handleRollbackConfig(w http.ResponseWriter, r *http.Request) {
	targetVersion, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || targetVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid_version", "config version must be a positive integer")
		return
	}
	instanceID := chi.URLParam(r, "instanceID")
	current, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if targetVersion == current.Version {
		writeError(w, http.StatusConflict, "already_current", "the requested config version is already active")
		return
	}
	config, err := s.store.ConfigAtVersion(r.Context(), instanceID, targetVersion)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-config-rollback", instanceID, "configure", map[string]any{
		"target_version": targetVersion,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	updated, job, reused, err := s.store.UpdateInstanceAndQueueWithFingerprint(r.Context(), instanceID, current.Name, &config, current.Version,
		principal.User.ID, r.Header.Get("Idempotency-Key"), nil, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.config.rolled_back", "instance", instanceID, map[string]any{
		"from_version": current.Version, "target_version": targetVersion, "new_version": updated.Version, "job_id": job.ID,
	})
	_, _ = s.events.Publish(r.Context(), Event{Type: "instance.updated", ResourceType: "instance", ResourceID: instanceID,
		Data: map[string]any{"instance": updated, "job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"instance": updated, "job_id": job.ID, "idempotent_replay": reused})
}

func diffInstanceConfig(before, after domain.InstanceConfig) map[string]configFieldChange {
	beforeFlat, afterFlat := map[string]any{}, map[string]any{}
	flattenJSON("", jsonMap(before), beforeFlat)
	flattenJSON("", jsonMap(after), afterFlat)
	changes := make(map[string]configFieldChange)
	for key, oldValue := range beforeFlat {
		if newValue, ok := afterFlat[key]; !ok || !jsonValuesEqual(oldValue, newValue) {
			changes[key] = configFieldChange{Before: oldValue, After: newValue}
		}
	}
	for key, newValue := range afterFlat {
		if _, ok := beforeFlat[key]; !ok {
			changes[key] = configFieldChange{After: newValue}
		}
	}
	return changes
}

func jsonMap(value any) map[string]any {
	raw, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return result
}

func flattenJSON(prefix string, value map[string]any, destination map[string]any) {
	for key, item := range value {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := item.(map[string]any); ok {
			flattenJSON(path, nested, destination)
			continue
		}
		destination[path] = item
	}
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

var managedFileAPIRoots = []string{"cfg", "addons", "logs", "left4dead2/cfg", "left4dead2/addons", "left4dead2/logs"}

var blockedManagedFileAPIExtensions = map[string]bool{
	".bat": true, ".cmd": true, ".com": true, ".dll": true, ".dylib": true,
	".exe": true, ".ps1": true, ".sh": true, ".smx": true, ".so": true,
	".vdf": true, ".vpk": true,
}

func validateManagedFileAPIPath(value string) (string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, ":") || strings.HasPrefix(value, "/") {
		return "", errors.New("a relative managed file path is required")
	}
	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("file path escapes the managed directories")
	}
	for _, root := range managedFileAPIRoots {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			if strings.EqualFold(pathpkg.Base(cleaned), "l4d2panel.cfg") {
				return "", errors.New("the generated l4d2panel.cfg is managed by structured settings")
			}
			return cleaned, nil
		}
	}
	return "", errors.New("file path is outside cfg, addons, and logs")
}

func validateManagedFileAPIMutationPath(value string) (string, error) {
	cleaned, err := validateManagedFileAPIPath(value)
	if err != nil {
		return "", err
	}
	if blockedManagedFileAPIExtensions[strings.ToLower(pathpkg.Ext(cleaned))] {
		return "", errors.New("executable and load-bearing files must be installed through package management")
	}
	return cleaned, nil
}

func (s *Server) handleInstanceFileContent(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	relative, err := validateManagedFileAPIPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	if _, err := s.store.GetInstance(r.Context(), instanceID); err != nil {
		s.fail(w, err)
		return
	}
	payload := map[string]any{"operation": "read", "relative_path": relative}
	latest, latestErr := s.store.LatestSuccessfulJob(r.Context(), instanceID, "file", payload)
	refreshID := ""
	if latestErr != nil || latest.FinishedAt == nil || time.Since(*latest.FinishedAt) > 5*time.Second {
		principal := principalFromContext(r.Context())
		key := "file-read:" + instanceID + ":" + relative + ":" + strconv.FormatInt(time.Now().Unix()/5, 10)
		job, _, queueErr := s.store.QueueInstanceJob(r.Context(), instanceID, "file", payload, principal.User.ID, key)
		if queueErr == nil {
			refreshID = job.ID
			s.dispatchSoon(job.NodeID)
		} else if !errors.Is(queueErr, ErrConflict) && latestErr != nil {
			s.fail(w, queueErr)
			return
		}
	}
	if latestErr != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": refreshID, "path": relative, "status": "loading"})
		return
	}
	result, err := unsealFileJobResult(s.config.MasterKey, latest.Result)
	if err != nil {
		s.fail(w, err)
		return
	}
	encoded := stringFromAny(result["content_base64"])
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		s.fail(w, errors.New("managed file result is corrupted"))
		return
	}
	response := map[string]any{"path": relative, "size": intFromAny(result["size"]),
		"sha256": stringFromAny(result["sha256"]), "fresh_at": latest.FinishedAt, "refresh_job_id": refreshID,
	}
	if utf8.Valid(raw) && !strings.ContainsRune(string(raw), '\x00') {
		response["content"] = string(raw)
	} else {
		response["content_base64"] = encoded
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleWriteInstanceFile(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	relative, err := validateManagedFileAPIMutationPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	var request struct {
		Content        string `json:"content,omitempty"`
		ContentBase64  string `json:"content_base64,omitempty"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	}
	if !decodeJSONWithLimit(w, r, &request, 12<<20) {
		return
	}
	if request.Content != "" && request.ContentBase64 != "" {
		writeError(w, http.StatusBadRequest, "invalid_file_content", "provide content or content_base64, not both")
		return
	}
	if request.Content == "" && request.ContentBase64 == "" {
		// An explicitly empty text file is represented by content_base64="" as
		// well, so accept it by setting content to an empty string in the job.
		request.Content = ""
	}
	if err := validateSHA256(request.ExpectedSHA256); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_precondition", err.Error())
		return
	}
	payload := map[string]any{"operation": "write", "relative_path": relative, "content": request.Content,
		"content_base64": request.ContentBase64, "expected_sha256": strings.ToLower(request.ExpectedSHA256)}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-file", instanceID, "write", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "file", payload); err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "file", payload, principal.User.ID,
		r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.file.write_requested", "instance", instanceID, map[string]any{"path": relative, "job_id": job.ID})
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleDeleteInstanceFile(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	relative, err := validateManagedFileAPIMutationPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	expected := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), "\"")
	if err := validateSHA256(expected); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_precondition", err.Error())
		return
	}
	payload := map[string]any{"operation": "delete", "relative_path": relative, "expected_sha256": strings.ToLower(expected)}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-file", instanceID, "delete", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "file", payload, principal.User.ID,
		r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.file.delete_requested", "instance", instanceID, map[string]any{"path": relative, "job_id": job.ID})
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func decodeJSONWithLimit(w http.ResponseWriter, r *http.Request, destination any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is invalid or too large")
		return false
	}
	return true
}

func validateSHA256(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) != 64 {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	return nil
}
