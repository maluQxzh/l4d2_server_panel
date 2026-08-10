package control

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleScanInstalledPackages(w http.ResponseWriter, r *http.Request) {
	instanceID := chi.URLParam(r, "instanceID")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_package_scan", "installed VPK scanning does not accept a request body or path")
		return
	}
	payload := map[string]any{"directory": "left4dead2/addons", "mode": "installed_vpk_v1"}
	requestFingerprint, err := keyedRequestFingerprint(s.config.MasterKey, "instance-package", instanceID, "scan", payload)
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
	if s.agents.Connected(instance.NodeID) && !s.agents.Supports(instance.NodeID, "package_scan_v1") {
		writeError(w, http.StatusConflict, "package_scan_not_supported", "target node Agent does not support installed VPK scanning")
		return
	}
	job, reused, err := s.store.QueueInstanceJobWithFingerprint(r.Context(), instanceID, "scan_packages", payload,
		principal.User.ID, idempotencyKey, requestFingerprint)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "instance.package.scan_requested", "instance", instanceID, map[string]any{"path": "left4dead2/addons", "job_id": job.ID})
	_, _ = s.events.Publish(r.Context(), Event{Type: "job.queued", ResourceType: "job", ResourceID: job.ID, Data: map[string]any{"job": job}}, true)
	s.dispatchSoon(job.NodeID)
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "idempotent_replay": reused})
}
