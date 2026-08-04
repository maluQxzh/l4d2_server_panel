CREATE TABLE restore_config_projections (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    applied_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);

INSERT INTO restore_config_projections(job_id,instance_id,applied_at)
SELECT source_job_id,instance_id,created_at
FROM config_versions
WHERE source_job_id IS NOT NULL;

DROP TABLE config_versions;
