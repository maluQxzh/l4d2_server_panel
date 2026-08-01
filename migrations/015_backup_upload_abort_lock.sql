-- Abandoning an upload only removes Agent staging files. It must not wait for
-- (or block) game-process writes for the same instance. The store serializes
-- this operation against chunk/restore creation with the upload advisory lock.
DROP INDEX IF EXISTS jobs_instance_active_idx;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND (status IN ('queued','dispatched','running')
           OR (status='canceled' AND result @> '{"cancel_pending":true}'::jsonb))
      AND type NOT IN (
          'a2s.info','a2s.players','rcon.players','list','check_update',
          'logs.search','logs.chunk','backup.chunk','backup.upload.abort'
      )
      AND NOT (type='file' AND payload->>'operation'='read');
