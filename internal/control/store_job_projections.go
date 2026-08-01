package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// ApplyPendingJobProjection serializes terminal result projection across
// duplicate Agent journal frames. The durable result marker continues to hold
// the per-instance write index while the individual idempotent projectors run.
// It is cleared only after every projector succeeds, and the Agent is ACKed
// only after that clearing statement commits.
func (s *Store) ApplyPendingJobProjection(ctx context.Context, id string) (domain.Job, bool, error) {
	release := s.lockJobProjection(id)
	defer release()

	current, err := s.GetJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if !jobProjectionPending(current) {
		return current, false, nil
	}
	if !requiresDeferredJobProjection(current.Type, current.Status) {
		return domain.Job{}, false, errors.New("job has an invalid terminal projection marker")
	}

	if err := s.ApplyJobResult(ctx, current); err != nil {
		return domain.Job{}, false, fmt.Errorf("apply job result: %w", err)
	}
	if err := s.ApplyUpdateCheckResult(ctx, current); err != nil {
		return domain.Job{}, false, fmt.Errorf("apply update check result: %w", err)
	}
	if current.Status == domain.JobFailed {
		if err := s.ApplyJobFailure(ctx, current); err != nil {
			return domain.Job{}, false, fmt.Errorf("apply job failure: %w", err)
		}
	}

	projected, err := scanJob(s.pool.QueryRow(ctx, `UPDATE jobs SET result=result-$2::text
		WHERE id=$1 AND result @> jsonb_build_object($2::text,true)
		RETURNING `+jobColumns, id, jobProjectionPendingField))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, errors.New("terminal projection marker changed unexpectedly")
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	return projected, true, nil
}
