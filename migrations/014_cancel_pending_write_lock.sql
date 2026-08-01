-- A dispatched or running task remains Agent-owned after an operator asks to
-- cancel it. Keep the instance write lock until the Agent reports its actual
-- terminal outcome and the Control clears cancel_pending.
--
-- Older schemas did not count cancel_pending rows in this lock. Refuse to
-- guess if such a database already contains two potentially executing writes;
-- an operator must let/cancel those jobs settle before retrying the upgrade.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM jobs
        WHERE instance_id IS NOT NULL
          AND (status IN ('queued','dispatched','running')
               OR (status='canceled' AND result @> '{"cancel_pending":true}'::jsonb))
          AND type NOT IN (
              'a2s.info','a2s.players','rcon.players','list','check_update',
              'logs.search','logs.chunk','backup.chunk'
          )
          AND NOT (type='file' AND payload->>'operation'='read')
        GROUP BY instance_id
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot install cancel-pending write lock while an instance has multiple unfinished writes'
            USING ERRCODE = '23505',
                  HINT = 'Let dispatched/running jobs report a terminal result and cancel queued duplicates, then retry the migration.';
    END IF;
END $$;

DROP INDEX IF EXISTS jobs_instance_active_idx;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND (status IN ('queued','dispatched','running')
           OR (status='canceled' AND result @> '{"cancel_pending":true}'::jsonb))
      AND type NOT IN (
          'a2s.info','a2s.players','rcon.players','list','check_update',
          'logs.search','logs.chunk','backup.chunk'
      )
      AND NOT (type='file' AND payload->>'operation'='read');
