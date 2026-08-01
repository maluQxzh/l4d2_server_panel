-- A terminal Agent result is durable before its Control-side projections run.
-- Keep write jobs in the per-instance exclusion index until every idempotent
-- projector has succeeded and Control removes _projection_pending. This closes
-- both the Control crash window and stale ACK-replay overwrites.
DROP INDEX IF EXISTS jobs_instance_active_idx;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND (status IN ('queued','dispatched','running')
           OR (status='canceled' AND result @> '{"cancel_pending":true}'::jsonb)
           OR (status IN ('succeeded','failed') AND result @> '{"_projection_pending":true}'::jsonb))
      AND type NOT IN (
          'a2s.info','a2s.players','rcon.players','list','check_update',
          'logs.search','logs.chunk','backup.chunk','backup.upload.abort'
      )
      AND NOT (type='file' AND payload->>'operation'='read');
