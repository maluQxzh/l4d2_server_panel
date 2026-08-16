CREATE TABLE job_terminal_conflicts (
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    reported_fingerprint TEXT NOT NULL CHECK (
        length(reported_fingerprint)=64 AND reported_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL DEFAULT '',
    stored_status TEXT NOT NULL CHECK (stored_status IN ('succeeded','failed','canceled')),
    reported_status TEXT NOT NULL CHECK (reported_status IN ('succeeded','failed','canceled')),
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 64),
    occurrences INTEGER NOT NULL DEFAULT 1 CHECK (occurrences>=1),
    first_seen_at TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP NOT NULL,
    resolution TEXT CHECK (resolution IS NULL OR resolution='keep_control'),
    resolved_at TIMESTAMP,
    resolved_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    resolution_idempotency_key TEXT,
    resolution_request_fingerprint TEXT CHECK (
        resolution_request_fingerprint IS NULL OR (
            length(resolution_request_fingerprint)=64 AND resolution_request_fingerprint NOT GLOB '*[^0-9a-f]*'
        )
    ),
    PRIMARY KEY(job_id,reported_fingerprint),
    CHECK ((resolution IS NULL AND resolved_at IS NULL AND resolved_by IS NULL
            AND resolution_idempotency_key IS NULL AND resolution_request_fingerprint IS NULL)
        OR (resolution='keep_control' AND resolved_at IS NOT NULL
            AND resolution_idempotency_key IS NOT NULL AND resolution_request_fingerprint IS NOT NULL))
);

CREATE INDEX job_terminal_conflicts_active_idx
    ON job_terminal_conflicts(last_seen_at DESC,job_id)
    WHERE resolution IS NULL;

CREATE UNIQUE INDEX job_terminal_conflicts_resolution_idempotency_idx
    ON job_terminal_conflicts(job_id,resolution_idempotency_key)
    WHERE resolution_idempotency_key IS NOT NULL;
