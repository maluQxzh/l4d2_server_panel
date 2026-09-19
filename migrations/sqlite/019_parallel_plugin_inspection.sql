-- Inspection only reads an isolated archive and returns a plan. It never
-- commits instance files or resource projections, so it must not own a write
-- barrier. Keep all mutation/cancellation/projection exclusions intact.
-- Virtual generated columns allow a forward-only replacement without rebuilding
-- jobs or disturbing foreign keys to existing job IDs. The lock remains indexed.
DROP INDEX jobs_instance_active_idx;
ALTER TABLE jobs DROP COLUMN holds_instance_write_lock;
ALTER TABLE jobs DROP COLUMN is_instance_write;

ALTER TABLE jobs ADD COLUMN is_instance_write INTEGER GENERATED ALWAYS AS (
    CASE WHEN instance_id IS NOT NULL
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort','inspect_plugin_archive')
      AND NOT (type='file' AND json_extract(payload,'$.operation')='read')
    THEN 1 ELSE 0 END
) VIRTUAL;
ALTER TABLE jobs ADD COLUMN holds_instance_write_lock INTEGER GENERATED ALWAYS AS (
    CASE WHEN is_instance_write=1
      AND (status IN ('queued','dispatched','running')
        OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1)
        OR (status IN ('succeeded','failed') AND json_extract(result,'$._projection_pending')=1))
    THEN 1 ELSE 0 END
) VIRTUAL;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id) WHERE holds_instance_write_lock=1;

-- A cancellation still consuming Agent resources must also retain this slot.
CREATE UNIQUE INDEX jobs_instance_inspection_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL AND type='inspect_plugin_archive'
      AND (status IN ('queued','dispatched','running')
        OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1));
