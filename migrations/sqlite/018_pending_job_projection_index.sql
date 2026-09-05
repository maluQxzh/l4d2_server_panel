-- Recover the durable terminal projection outbox without scanning all job
-- results every housekeeping tick. Active work and unconfirmed cancellations
-- must never be reconciled from a guessed terminal outcome.
CREATE INDEX jobs_pending_projection_idx ON jobs(id)
WHERE status IN ('succeeded','failed','canceled')
  AND COALESCE(json_extract(result,'$.cancel_pending'),0)<>1
  AND json_extract(result,'$._projection_pending')=1;
