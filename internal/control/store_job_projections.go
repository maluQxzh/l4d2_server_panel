package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	if !jobProjectionPending(current) {
		return current, false, nil
	}
	cleanedPayload, payloadChanged := scrubTerminalJobPayload(current.Payload)
	if payloadChanged {
		payloadRaw, marshalErr := json.Marshal(cleanedPayload)
		if marshalErr != nil {
			return domain.Job{}, false, marshalErr
		}
		current, err = scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs SET payload=?2
			WHERE id=?1 AND json_extract(result,'$._projection_pending')=1
			RETURNING `+jobColumns, id, string(payloadRaw)))
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, false, errors.New("terminal projection marker changed unexpectedly")
		}
		if err != nil {
			return domain.Job{}, false, fmt.Errorf("scrub terminal job payload: %w", err)
		}
	}

	if requiresDeferredJobProjection(current.Type, current.Payload, current.Status) {
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
		// Some projectors enrich the durable task result (for example with a
		// quarantine entry). Reload before clearing the barrier so the compact
		// replacement cannot overwrite those committed fields with the stale
		// pre-projection snapshot.
		current, err = s.GetJob(ctx, id)
		if err != nil {
			return domain.Job{}, false, fmt.Errorf("reload projected job: %w", err)
		}
	}

	settledResult := cloneMap(current.Result)
	delete(settledResult, jobProjectionPendingField)
	if current.Type == "map.import.qq_flash" {
		delete(settledResult, "poster_base64")
		delete(settledResult, "poster_mime")
	}
	resultRaw, err := json.Marshal(settledResult)
	if err != nil {
		return domain.Job{}, false, err
	}
	projected, err := scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs SET result=?2
		WHERE id=?1 AND json_extract(result,'$._projection_pending')=1
		RETURNING `+jobColumns, id, string(resultRaw)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, errors.New("terminal projection marker changed unexpectedly")
	}
	if err != nil {
		return domain.Job{}, false, err
	}
	return projected, true, nil
}
