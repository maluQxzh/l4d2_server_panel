CREATE TABLE system_update_jobs (
    id TEXT PRIMARY KEY,
    requested_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    status TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed','canceled')),
    phase TEXT NOT NULL CHECK (phase IN ('validating','stopping','backing_up','installing','migrating','starting','health_check','rolling_back')),
    current_version TEXT NOT NULL,
    target_version TEXT NOT NULL DEFAULT '',
    package_sha256 TEXT NOT NULL DEFAULT '' CHECK (package_sha256='' OR (length(package_sha256)=64 AND package_sha256 NOT GLOB '*[^0-9a-f]*')),
    upload_bytes INTEGER NOT NULL DEFAULT 0 CHECK (upload_bytes>=0),
    rollback TEXT NOT NULL DEFAULT '' CHECK (rollback IN ('','not_required','succeeded','failed')),
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP
);
CREATE INDEX system_update_jobs_created_idx ON system_update_jobs(created_at DESC);
