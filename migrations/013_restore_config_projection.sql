ALTER TABLE config_versions
    ADD COLUMN IF NOT EXISTS source_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS config_versions_source_job_idx
    ON config_versions(source_job_id);

COMMENT ON COLUMN config_versions.source_job_id IS
    'Terminal restore job whose portable launch configuration produced this version; prevents replay from incrementing twice.';
