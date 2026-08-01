CREATE TABLE IF NOT EXISTS quarantine_entries (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    source_job_id uuid NOT NULL UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
    original_instance_id uuid NOT NULL,
    instance_name text NOT NULL CHECK (char_length(instance_name) BETWEEN 1 AND 200),
    root_index integer NOT NULL CHECK (root_index >= 0),
    entry_name text NOT NULL CHECK (
        char_length(entry_name) BETWEEN 1 AND 200
        AND entry_name ~ '^[A-Za-z0-9._-]+-[0-9]{8}T[0-9]{6}Z$'
    ),
    quarantined_at timestamptz NOT NULL,
    retention_expires_at timestamptz NOT NULL,
    purge_job_id uuid REFERENCES jobs(id) ON DELETE SET NULL,
    purged_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(node_id, root_index, entry_name),
    CHECK (retention_expires_at = quarantined_at + interval '168 hours'),
    CHECK (purged_at IS NULL OR purged_at >= quarantined_at)
);
CREATE INDEX IF NOT EXISTS quarantine_entries_node_active_idx
    ON quarantine_entries(node_id, quarantined_at DESC)
    WHERE purged_at IS NULL;

CREATE TABLE IF NOT EXISTS quarantine_purge_confirmations (
    id uuid PRIMARY KEY,
    quarantine_entry_id uuid NOT NULL REFERENCES quarantine_entries(id) ON DELETE CASCADE,
    requested_by uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    job_id uuid REFERENCES jobs(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);
CREATE INDEX IF NOT EXISTS quarantine_purge_confirmations_expiry_idx
    ON quarantine_purge_confirmations(expires_at)
    WHERE consumed_at IS NULL;
