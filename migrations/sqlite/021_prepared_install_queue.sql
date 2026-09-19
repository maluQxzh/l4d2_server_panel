-- Control-owned coordinator jobs never hold the instance write barrier.
DROP INDEX jobs_instance_active_idx;
ALTER TABLE jobs DROP COLUMN holds_instance_write_lock;
ALTER TABLE jobs DROP COLUMN is_instance_write;
ALTER TABLE jobs ADD COLUMN is_instance_write INTEGER GENERATED ALWAYS AS (
    CASE WHEN instance_id IS NOT NULL
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort','inspect_plugin_archive','prepare_package','discard_prepared_package','prepared_install_queue')
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

CREATE TABLE prepared_install_queues (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    -- Retain token identity after revocation/deletion, to fail closed.
    api_token_id TEXT NOT NULL DEFAULT '',
    active INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1))
);
CREATE UNIQUE INDEX prepared_install_queue_active_idx ON prepared_install_queues(instance_id) WHERE active=1;
CREATE TABLE prepared_install_queue_items (
    queue_id TEXT NOT NULL REFERENCES prepared_install_queues(job_id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK(position BETWEEN 0 AND 15),
    prepared_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    install_job_id TEXT UNIQUE REFERENCES jobs(id) ON DELETE SET NULL,
    state TEXT NOT NULL DEFAULT 'waiting' CHECK(state IN ('waiting','submitted','skipped')),
    PRIMARY KEY(queue_id,position),
    UNIQUE(queue_id,prepared_id)
);
CREATE UNIQUE INDEX prepared_install_waiting_claim_idx ON prepared_install_queue_items(prepared_id) WHERE state='waiting';
