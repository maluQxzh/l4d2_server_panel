package control

import (
	"context"
	"errors"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// The persisted terminal result is an outbox, not just an Agent replay hint.
// Recover it even if the Agent is offline or no longer has the original journal
// entry. Never fabricate an outcome, cancel active work, or send an ACK here:
// a later Agent replay must still pass the normal immutable-outcome comparison.
func (s *Server) reconcilePendingJobProjections(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	const batchSize = 50
	ids, err := s.store.PendingJobProjectionIDs(ctx, s.projectionRecoveryCursor, batchSize)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("load pending job projections", "error", err)
		}
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.projectionRecoveryCursor = id
		original, err := s.store.GetJob(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		var projected domain.Job
		var applied bool
		if err == nil {
			projected, applied, err = s.store.ApplyPendingJobProjection(ctx, id)
		}
		if err != nil {
			s.logger.Warn("recover terminal job projection", "job_id", id, "error", err)
			continue
		}
		if !applied {
			continue
		}
		if err := s.publishRecoveredJobProjection(ctx, original, projected); err != nil {
			s.logger.Warn("publish recovered job projection", "job_id", id, "error", err)
		}
	}
	if len(ids) < batchSize {
		s.projectionRecoveryCursor = ""
	}
}

func (s *Server) publishRecoveredJobProjection(ctx context.Context, original, projected domain.Job) error {
	s.agents.forgetCompleted(projected.NodeID, projected.ID)
	deleted := projected.Status == domain.JobSucceeded && (projected.Type == "delete" || projected.Type == "detach")
	if deleted && original.InstanceID != "" {
		s.agents.invalidateInstanceOwnership(original.InstanceID)
		s.events.InvalidateAuthorization()
		if _, err := s.events.Publish(ctx, Event{Type: "instance.deleted", ResourceType: "instance", ResourceID: original.InstanceID,
			Data: map[string]any{"instance_id": original.InstanceID, "node_id": original.NodeID, "job_id": original.ID}}, true); err != nil {
			return err
		}
	} else if original.InstanceID != "" && jobChangesInstance(projected.Type) {
		if err := s.publishInstanceProjection(ctx, original.InstanceID, projected.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	projected.Payload, projected.Result = cloneMap(projected.Payload), cloneMap(projected.Result)
	stripJobFields(projected.Type, projected.Payload, projected.Result)
	_, err := s.events.Publish(ctx, Event{Type: "job.updated", ResourceType: "job", ResourceID: projected.ID,
		Data: map[string]any{"job": projected}}, true)
	return err
}
