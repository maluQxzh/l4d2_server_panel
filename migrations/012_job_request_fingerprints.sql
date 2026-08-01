ALTER TABLE jobs ADD COLUMN IF NOT EXISTS request_fingerprint text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'jobs'::regclass AND conname = 'jobs_request_fingerprint_format'
    ) THEN
        ALTER TABLE jobs ADD CONSTRAINT jobs_request_fingerprint_format
            CHECK (request_fingerprint IS NULL OR request_fingerprint ~ '^[0-9a-f]{64}$');
    END IF;
END $$;

COMMENT ON COLUMN jobs.request_fingerprint IS
    'Internal SHA-256/HMAC-SHA256 identity of the canonical request. NULL legacy rows never qualify for an idempotent replay.';
