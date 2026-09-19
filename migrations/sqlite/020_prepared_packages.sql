DROP INDEX jobs_instance_active_idx;
ALTER TABLE jobs DROP COLUMN holds_instance_write_lock;
ALTER TABLE jobs DROP COLUMN is_instance_write;
ALTER TABLE jobs ADD COLUMN is_instance_write INTEGER GENERATED ALWAYS AS (
    CASE WHEN instance_id IS NOT NULL
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort','inspect_plugin_archive','prepare_package','discard_prepared_package')
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
-- A prepared package has a single consuming intent, even if that task fails.
-- Retries with the original idempotency key still return the original job.
CREATE UNIQUE INDEX jobs_prepared_package_claim_idx ON jobs(json_extract(payload,'$.prepared_package_id'))
    WHERE type IN ('install_mod','discard_prepared_package') AND json_type(payload,'$.prepared_package_id')='text';
CREATE INDEX jobs_prepared_package_list_idx ON jobs(instance_id,created_at DESC) WHERE type='prepare_package';
