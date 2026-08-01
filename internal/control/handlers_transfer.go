package control

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const maxHTTPTransferChunk = 1 << 20

type chunkRequest struct {
	Segment        int    `json:"segment,omitempty"`
	Offset         int64  `json:"offset"`
	Length         int    `json:"length"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

func (s *Server) handleCreateLogSearch(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	var request struct {
		Since   string `json:"since,omitempty"`
		Until   string `json:"until,omitempty"`
		Keyword string `json:"keyword,omitempty"`
		Limit   int    `json:"limit,omitempty"`
	}
	if !decodeJSONWithLimit(w, r, &request, 16<<10) {
		return
	}
	request.Keyword = strings.TrimSpace(request.Keyword)
	if len(request.Keyword) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_log_query", "keyword exceeds 256 bytes")
		return
	}
	if request.Limit == 0 {
		request.Limit = 200
	}
	if request.Limit < 1 || request.Limit > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_log_query", "limit must be between 1 and 1000")
		return
	}
	since, err := parseOptionalTransferTime(request.Since)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_log_query", "since must be RFC3339")
		return
	}
	until, err := parseOptionalTransferTime(request.Until)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_log_query", "until must be RFC3339")
		return
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		writeError(w, http.StatusBadRequest, "invalid_log_query", "since must not be after until")
		return
	}
	payload := map[string]any{"keyword": request.Keyword, "limit": request.Limit}
	if !since.IsZero() {
		payload["since"] = since.UTC().Format(time.RFC3339Nano)
	}
	if !until.IsZero() {
		payload["until"] = until.UTC().Format(time.RFC3339Nano)
	}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "logs.search", payload); err != nil {
		s.fail(w, err)
		return
	}
	s.queueTransferJob(w, r, instanceID, "logs.search", payload, "instance.logs.search_requested")
}

func (s *Server) handleGetLogSearch(w http.ResponseWriter, r *http.Request) {
	job, ok := s.transferJobForResponse(w, r, "logs.search")
	if !ok {
		return
	}
	result, err := unsealTransferJobResult(s.config.MasterKey, job.Type, job.Result)
	if err != nil {
		if errors.Is(err, errTransferResultConsumed) {
			writeError(w, http.StatusGone, "transfer_consumed", "log search result was already consumed; create a new search")
			return
		}
		s.fail(w, err)
		return
	}
	if err := s.store.ConsumeJobSecretResult(r.Context(), job.ID, job.Type, "entries_ciphertext"); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": result["entries"], "returned": result["returned"],
		"matched": result["matched"], "truncated": result["truncated"]})
}

func (s *Server) handleCreateLogDownload(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	var request chunkRequest
	if !decodeJSONWithLimit(w, r, &request, 8<<10) || !validateChunkRequest(w, &request, true) {
		return
	}
	payload := map[string]any{"segment": request.Segment, "offset": request.Offset, "length": request.Length,
		"expected_sha256": strings.ToLower(request.ExpectedSHA256)}
	s.queueTransferJob(w, r, instanceID, "logs.chunk", payload, "instance.logs.download_requested")
}

func (s *Server) handleGetLogDownload(w http.ResponseWriter, r *http.Request) {
	job, ok := s.transferJobForResponse(w, r, "logs.chunk")
	if !ok {
		return
	}
	s.writeChunkJobResponse(w, r, job, "instance-"+job.InstanceID+".log", "", 0)
}

func (s *Server) handleCreateBackupDownload(w http.ResponseWriter, r *http.Request) {
	instanceID, backupID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "backupID")
	backup, err := s.store.GetBackup(r.Context(), instanceID, backupID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var request chunkRequest
	if !decodeJSONWithLimit(w, r, &request, 8<<10) || !validateChunkRequest(w, &request, false) {
		return
	}
	if request.Offset >= int64(backup.Size) {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "offset is beyond the backup size")
		return
	}
	payload := map[string]any{"backup_id": backup.ID, "archive": backup.Path, "expected_size": backup.Size,
		"expected_sha256": strings.ToLower(backup.SHA256), "offset": request.Offset, "length": request.Length}
	if err := sealJobPayloadSecrets(s.config.MasterKey, "backup.chunk", payload); err != nil {
		s.fail(w, err)
		return
	}
	s.queueTransferJob(w, r, instanceID, "backup.chunk", payload, "instance.backup.download_requested")
}

func (s *Server) handleGetBackupDownload(w http.ResponseWriter, r *http.Request) {
	instanceID, backupID := chi.URLParam(r, "instanceID"), chi.URLParam(r, "backupID")
	backup, err := s.store.GetBackup(r.Context(), instanceID, backupID)
	if err != nil {
		s.fail(w, err)
		return
	}
	job, ok := s.transferJobForResponse(w, r, "backup.chunk")
	if !ok {
		return
	}
	if stringFromAny(job.Payload["backup_id"]) != backup.ID || int64FromAny(job.Payload["expected_size"]) != int64(backup.Size) ||
		!strings.EqualFold(stringFromAny(job.Payload["expected_sha256"]), backup.SHA256) {
		writeError(w, http.StatusConflict, "backup_changed", "backup metadata no longer matches the download task")
		return
	}
	s.writeChunkJobResponse(w, r, job, backup.Name, backup.SHA256, int64(backup.Size))
}

func (s *Server) queueTransferJob(w http.ResponseWriter, r *http.Request, instanceID, jobType string, payload map[string]any, auditAction string) {
	principal := principalFromContext(r.Context())
	job, _, err := s.store.QueueInstanceJob(r.Context(), instanceID, jobType, payload, principal.User.ID, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	if !principal.AllowsJob(job) {
		writeTokenScopeDenied(w)
		return
	}
	s.audit(r, auditAction, "instance", instanceID, map[string]any{"job_id": job.ID})
	s.dispatchSoon(job.NodeID)
	w.Header().Set("Location", transferPollPath(r.URL.Path, job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status,
		"poll_url": transferPollPath(r.URL.Path, job.ID)})
}

func transferPollPath(createPath, jobID string) string {
	return strings.TrimSuffix(createPath, "/") + "/" + jobID
}

func (s *Server) transferJobForResponse(w http.ResponseWriter, r *http.Request, expectedType string) (domain.Job, bool) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return domain.Job{}, false
	}
	if job.Type != expectedType || job.InstanceID != chi.URLParam(r, "instanceID") || !principalFromContext(r.Context()).AllowsJob(job) {
		writeError(w, http.StatusNotFound, "not_found", "download task not found")
		return domain.Job{}, false
	}
	switch job.Status {
	case domain.JobQueued, domain.JobDispatched, domain.JobRunning:
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status, "progress": job.Progress})
		return domain.Job{}, false
	case domain.JobFailed, domain.JobCanceled:
		writeError(w, http.StatusConflict, "transfer_failed", firstNonEmptyString(job.Message, "transfer task did not complete"))
		return domain.Job{}, false
	case domain.JobSucceeded:
		return job, true
	default:
		writeError(w, http.StatusConflict, "transfer_failed", "transfer task is in an invalid state")
		return domain.Job{}, false
	}
}

func (s *Server) writeChunkJobResponse(w http.ResponseWriter, r *http.Request, job domain.Job, filename, expectedSHA string, expectedSize int64) {
	result, err := unsealTransferJobResult(s.config.MasterKey, job.Type, job.Result)
	if err != nil {
		if errors.Is(err, errTransferResultConsumed) {
			writeError(w, http.StatusGone, "transfer_consumed", "download chunk was already consumed; create a new chunk task")
			return
		}
		s.fail(w, err)
		return
	}
	encoded := stringFromAny(result["content_base64"])
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) > maxHTTPTransferChunk {
		s.fail(w, errors.New("transfer chunk is corrupted or exceeds the size limit"))
		return
	}
	digest := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), stringFromAny(result["chunk_sha256"])) {
		s.fail(w, errors.New("transfer chunk checksum mismatch"))
		return
	}
	offset, total := int64FromAny(result["offset"]), int64FromAny(result["total_size"])
	if offset < 0 || total < 0 || offset+int64(len(raw)) > total || intFromAny(result["length"]) != len(raw) {
		s.fail(w, errors.New("transfer chunk metadata is invalid"))
		return
	}
	if (expectedSize > 0 && total != expectedSize) || (expectedSHA != "" && !strings.EqualFold(expectedSHA, stringFromAny(result["file_sha256"]))) {
		writeError(w, http.StatusConflict, "transfer_source_changed", "transfer result no longer matches the requested file")
		return
	}
	if err := s.store.ConsumeJobSecretResult(r.Context(), job.ID, job.Type, "content_base64_ciphertext"); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeDownloadName(filename)}))
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("X-Chunk-SHA256", stringFromAny(result["chunk_sha256"]))
	w.Header().Set("X-File-SHA256", stringFromAny(result["file_sha256"]))
	w.Header().Set("X-Next-Offset", strconv.FormatInt(int64FromAny(result["next_offset"]), 10))
	w.Header().Set("X-Transfer-EOF", strconv.FormatBool(boolFromAny(result["eof"])))
	w.Header().Set("ETag", `"`+stringFromAny(result["file_sha256"])+`"`)
	if len(raw) > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(raw))-1, total))
	}
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(raw)
}

func validateChunkRequest(w http.ResponseWriter, request *chunkRequest, allowSegment bool) bool {
	if request.Length == 0 {
		request.Length = maxHTTPTransferChunk
	}
	if request.Offset < 0 || request.Length < 1 || request.Length > maxHTTPTransferChunk {
		writeError(w, http.StatusBadRequest, "invalid_range", "offset must be non-negative and length between 1 and 1048576")
		return false
	}
	if allowSegment && (request.Segment < 0 || request.Segment > 19) {
		writeError(w, http.StatusBadRequest, "invalid_log_segment", "segment must be between 0 and 19")
		return false
	}
	if !allowSegment && request.Segment != 0 {
		writeError(w, http.StatusBadRequest, "invalid_range", "backup downloads do not accept a segment")
		return false
	}
	if err := validateSHA256(request.ExpectedSHA256); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_checksum", err.Error())
		return false
	}
	return true
}

func parseOptionalTransferTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case uint64:
		if typed <= uint64(^uint64(0)>>1) {
			return int64(typed)
		}
	case float64:
		return int64(typed)
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	}
	return 0
}

func boolFromAny(value any) bool {
	result, _ := value.(bool)
	return result
}

func safeDownloadName(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(strings.ReplaceAll(value, "/", "_"), "\\", "_")
	if value == "" {
		return "download.bin"
	}
	runes := []rune(value)
	if len(runes) > 180 {
		value = string(runes[:180])
	}
	return value
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
