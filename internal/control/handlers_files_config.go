package control

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

var managedFileAPIRoots = []string{"cfg", "addons", "logs", "left4dead2/cfg", "left4dead2/addons", "left4dead2/logs"}

var blockedManagedFileAPIExtensions = map[string]bool{
	".bat": true, ".cmd": true, ".com": true, ".dll": true, ".dylib": true,
	".exe": true, ".ps1": true, ".sh": true, ".smx": true, ".so": true,
	".vdf": true, ".vpk": true,
}

var qqFlashShareURLPattern = regexp.MustCompile(`https://qfile\.qq\.com/q/[A-Za-z0-9_-]+`)

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
				return "", errors.New("legacy l4d2panel.cfg may contain secrets and is not exposed through file management")
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

func (s *Server) handleImportQQFlashFile(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	var request struct {
		// RelativePath is accepted only for a cached previous-minor UI. It is
		// deliberately ignored; both Control and Agent fix the destination.
		RelativePath string `json:"relative_path,omitempty"`
		ShareURL     string `json:"share_url"`
	}
	if !decodeJSONWithLimit(w, r, &request, 4<<10) {
		return
	}
	shareURL, err := normalizeQQFlashShareURL(request.ShareURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_qq_flash_share_url", err.Error())
		return
	}
	payload := map[string]any{"source": shareURL}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-package", instanceID, "qq_flash", payload)
	if err != nil {
		s.fail(w, err)
		return
	}
	principal := principalFromContext(r.Context())
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
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "qq_flash_package_v1") {
		writeError(w, http.StatusConflict, "qq_flash_not_supported", "target node Agent does not support QQ flash file imports")
		return
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "qq_flash_import", payload); err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "qq_flash_import", payload, principal.User.ID,
		idempotencyKey, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.package.qq_flash_requested", "instance", instanceID, map[string]any{"path": "left4dead2/addons", "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func normalizeQQFlashShareURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 {
		return "", errors.New("a QQ flash share link is required")
	}
	matches := qqFlashShareURLPattern.FindAllString(value, -1)
	unique := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		unique[match] = struct{}{}
	}
	if len(unique) != 1 {
		return "", errors.New("provide exactly one https://qfile.qq.com/q/... share link")
	}
	var normalized string
	for match := range unique {
		normalized = match
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "qfile.qq.com" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("QQ flash share link is invalid")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 2 || parts[0] != "q" || len(parts[1]) < 6 || len(parts[1]) > 64 {
		return "", errors.New("QQ flash share code is invalid")
	}
	return normalized, nil
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
