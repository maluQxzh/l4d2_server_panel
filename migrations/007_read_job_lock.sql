-- Transfer jobs and update probes are read-only and must not acquire the
-- per-instance write-operation lock. Migration 005 added transfer jobs but
-- accidentally omitted check_update from this exclusion.
DROP INDEX IF EXISTS jobs_instance_active_idx;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND status IN ('queued','dispatched','running')
      AND type NOT IN (
          'a2s.info','a2s.players','rcon.players','list','check_update',
          'logs.search','logs.chunk','backup.chunk'
      )
      AND NOT (type='file' AND payload->>'operation'='read');
