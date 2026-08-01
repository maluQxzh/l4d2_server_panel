CREATE TABLE IF NOT EXISTS api_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    scopes text[] NOT NULL,
    node_id uuid REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id uuid REFERENCES instances(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    last_used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (cardinality(scopes) BETWEEN 1 AND 3),
    CHECK (scopes <@ ARRAY['read', 'operate', 'manage']::text[]),
    CHECK (octet_length(token_hash) = 32)
);
CREATE INDEX IF NOT EXISTS api_tokens_user_created_idx ON api_tokens(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS api_tokens_expires_idx ON api_tokens(expires_at)
    WHERE revoked_at IS NULL AND expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS totp_recovery_codes (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash bytea NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(user_id, code_hash),
    CHECK (octet_length(code_hash) = 32)
);
CREATE INDEX IF NOT EXISTS totp_recovery_codes_user_unused_idx ON totp_recovery_codes(user_id)
    WHERE used_at IS NULL;
