CREATE TABLE IF NOT EXISTS player_bans (
    id uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    steam_id text NOT NULL,
    ip_address inet,
    reason text NOT NULL DEFAULT '',
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(instance_id,steam_id)
);

CREATE INDEX IF NOT EXISTS player_bans_instance_expiry_idx ON player_bans(instance_id,expires_at);
