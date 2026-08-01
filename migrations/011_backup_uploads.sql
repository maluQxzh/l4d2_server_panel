CREATE TABLE IF NOT EXISTS backup_uploads (
    id uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 180),
    platform text NOT NULL CHECK (platform IN ('windows/amd64','linux/amd64')),
    size bigint NOT NULL CHECK (size > 0 AND size <= 21474836480),
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    received_size bigint NOT NULL DEFAULT 0 CHECK (received_size >= 0 AND received_size <= size),
    status text NOT NULL DEFAULT 'uploading'
        CHECK (status IN ('uploading','ready','restoring','succeeded','failed','expired')),
    requested_by uuid REFERENCES users(id) ON DELETE SET NULL,
    create_idempotency_key text,
    last_chunk_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL,
    restore_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instance_id, id)
);

CREATE INDEX IF NOT EXISTS backup_uploads_instance_created_idx
    ON backup_uploads(instance_id, created_at DESC);
CREATE INDEX IF NOT EXISTS backup_uploads_expiry_idx
    ON backup_uploads(expires_at)
    WHERE status IN ('uploading','ready');
CREATE UNIQUE INDEX IF NOT EXISTS backup_uploads_create_idempotency_idx
    ON backup_uploads(requested_by, create_idempotency_key)
    WHERE requested_by IS NOT NULL
      AND create_idempotency_key IS NOT NULL
      AND create_idempotency_key <> '';

CREATE INDEX IF NOT EXISTS jobs_backup_upload_active_idx
    ON jobs ((payload->>'upload_id'))
    WHERE type='backup.upload.chunk' AND status IN ('queued','dispatched','running','succeeded');
