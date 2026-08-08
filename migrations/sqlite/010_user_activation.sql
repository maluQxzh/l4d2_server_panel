ALTER TABLE users ADD COLUMN account_state TEXT NOT NULL DEFAULT 'active'
    CHECK (account_state IN ('pending_activation','active'));

ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'standard'
    CHECK (kind IN ('standard','activation'));

CREATE TABLE user_activation_tokens (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash)=32),
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX user_activation_tokens_expires_at_idx ON user_activation_tokens(expires_at);
