package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type discoveryRequest struct {
	MaxDepth   int `json:"max_depth,omitempty"`
	MaxEntries int `json:"max_entries,omitempty"`
}

type discoveryCandidateResponse struct {
	RootIndex      int      `json:"root_index"`
	Path           string   `json:"path"`
	Name           string   `json:"name"`
	Executable     string   `json:"executable"`
	HasGameInfo    bool     `json:"has_gameinfo"`
	HasAppManifest bool     `json:"has_appmanifest"`
	Managed        bool     `json:"managed"`
	LikelyRunning  bool     `json:"likely_running"`
	PID            int32    `json:"pid,omitempty"`
	Evidence       []string `json:"evidence"`
}

type discoveryResultResponse struct {
	Candidates         []discoveryCandidateResponse `json:"candidates"`
	ScannedDirectories int                          `json:"scanned_directories"`
	SkippedLinks       int                          `json:"skipped_links"`
	Truncated          bool                         `json:"truncated"`
}

func validateDiscoveryPortableComponent(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || strings.TrimRight(value, ". ") != value {
		return errors.New("value is not a portable path component")
	}
	for _, character := range value {
		if character == 0 || character < 0x20 || character == 0x7f || strings.ContainsRune(`<>:"|?*`, character) {
			return errors.New("value is not a portable path component")
		}
	}
	base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return errors.New("value is a reserved Windows device name")
	}
	return nil
}

func validateDiscoveryCandidatePath(value string) error {
	if value == "." {
		return nil
	}
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) {
		return errors.New("discovery candidate path must be a slash-separated relative path")
	}
	cleaned := pathpkg.Clean(value)
	if cleaned != value || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("discovery candidate path must not contain traversal or ambiguous components")
	}
	for _, component := range strings.Split(cleaned, "/") {
		if err := validateDiscoveryPortableComponent(component); err != nil {
			return fmt.Errorf("discovery candidate path contains an unsafe component: %w", err)
		}
	}
	return nil
}

func validDiscoveryExecutable(value string) bool {
	switch value {
	case "srcds.exe", "srcds_run", "srcds_linux":
		return true
	default:
		return false
	}
}

func validateDiscoveryEvidence(candidate discoveryCandidateResponse) error {
	expected := []string{candidate.Executable, "left4dead2/"}
	if candidate.HasGameInfo {
		expected = append(expected, "left4dead2/gameinfo.txt")
	}
	if candidate.HasAppManifest {
		expected = append(expected, "appmanifest_222860.acf")
	}
	if len(candidate.Evidence) != len(expected) {
		return errors.New("discovery evidence does not match the fixed Agent projection")
	}
	for index := range expected {
		if candidate.Evidence[index] != expected[index] {
			return errors.New("discovery evidence does not match the fixed Agent projection")
		}
	}
	return nil
}

func fixedDiscoveryJobResult(job domain.Job) (domain.Job, error) {
	sourceResult := job.Result
	job.Payload = nil
	job.Result = nil
	job.IdempotencyKey = ""
	job.RequestedBy = ""
	job.Message = ""
	if job.Status != domain.JobSucceeded {
		return job, nil
	}
	raw, err := json.Marshal(sourceResult)
	if err != nil {
		return domain.Job{}, err
	}
	var result discoveryResultResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return domain.Job{}, fmt.Errorf("decode discovery result: %w", err)
	}
	if len(result.Candidates) > 200 || result.ScannedDirectories < 0 || result.SkippedLinks < 0 {
		return domain.Job{}, errors.New("discovery result is outside the supported bounds")
	}
	if result.Candidates == nil {
		result.Candidates = make([]discoveryCandidateResponse, 0)
	}
	for index := range result.Candidates {
		candidate := &result.Candidates[index]
		if candidate.RootIndex < 0 || candidate.PID < 0 || len(candidate.Path) > 4096 || len(candidate.Name) > 512 ||
			len(candidate.Executable) > 512 || len(candidate.Evidence) > 32 {
			return domain.Job{}, errors.New("discovery candidate is outside the supported bounds")
		}
		if err := validateDiscoveryCandidatePath(candidate.Path); err != nil {
			return domain.Job{}, fmt.Errorf("invalid discovery candidate path: %w", err)
		}
		if err := validateDiscoveryPortableComponent(candidate.Name); err != nil {
			return domain.Job{}, fmt.Errorf("invalid discovery candidate name: %w", err)
		}
		if candidate.Path == "." {
			candidate.Name = "L4D2 Dedicated Server"
		} else if candidate.Name != pathpkg.Base(candidate.Path) {
			return domain.Job{}, errors.New("discovery candidate name does not match its relative path")
		}
		if !validDiscoveryExecutable(candidate.Executable) {
			return domain.Job{}, errors.New("discovery candidate executable is not supported")
		}
		if err := validateDiscoveryEvidence(*candidate); err != nil {
			return domain.Job{}, err
		}
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return domain.Job{}, err
	}
	if err := json.Unmarshal(raw, &job.Result); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func normalizeDiscoveryRequest(request discoveryRequest) (discoveryRequest, error) {
	if request.MaxDepth == 0 {
		request.MaxDepth = 6
	}
	if request.MaxEntries == 0 {
		request.MaxEntries = 5000
	}
	if request.MaxDepth < 1 || request.MaxDepth > 8 {
		return request, errors.New("max_depth must be between 1 and 8")
	}
	if request.MaxEntries < 1 || request.MaxEntries > 20000 {
		return request, errors.New("max_entries must be between 1 and 20000")
	}
	return request, nil
}

func (s *Server) handleCreateNodeDiscovery(w http.ResponseWriter, r *http.Request) {
	var request discoveryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request, err := normalizeDiscoveryRequest(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_discovery_limits", err.Error())
		return
	}
	nodeID := chi.URLParam(r, "nodeID")
	node, err := s.store.GetNode(r.Context(), nodeID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if node.Status != domain.NodeOnline || !s.agents.Connected(nodeID) {
		writeError(w, http.StatusConflict, "node_offline", "node must be online to scan instance roots")
		return
	}
	if !s.agents.Supports(nodeID, "discover_instances") {
		writeError(w, http.StatusConflict, "agent_upgrade_required", "connected Agent does not support instance discovery")
		return
	}
	principal := principalFromContext(r.Context())
	payload := map[string]any{"max_depth": request.MaxDepth, "max_entries": request.MaxEntries}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "node-discovery", nodeID, "discover_instances", request)
	if err != nil {
		s.fail(w, err)
		return
	}
	job, reused, err := s.store.QueueNodeJobWithFingerprint(r.Context(), nodeID, "discover_instances", payload, principal.User.ID,
		r.Header.Get("Idempotency-Key"), requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "node.discovery.requested", "node", nodeID, map[string]any{"job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(nodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}

func (s *Server) handleGetNodeDiscovery(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if job.NodeID != chi.URLParam(r, "nodeID") || job.InstanceID != "" || job.Type != "discover_instances" {
		writeError(w, http.StatusNotFound, "not_found", "discovery task not found")
		return
	}
	principal := principalFromContext(r.Context())
	if !principal.AllowsNodePermission(job.NodeID, PermissionNodesDiscover) {
		writeError(w, http.StatusForbidden, "forbidden", "node discovery permission denied")
		return
	}
	job, err = fixedDiscoveryJobResult(job)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}
