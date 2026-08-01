DROP INDEX IF EXISTS jobs_instance_active_idx;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND status IN ('queued', 'dispatched', 'running')
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','logs.search','logs.chunk','backup.chunk');
