CREATE TABLE IF NOT EXISTS users (
    id uuid PRIMARY KEY,
    username text NOT NULL,
    username_normalized text NOT NULL UNIQUE,
    display_name text NOT NULL,
    password_hash text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'operator', 'viewer')),
    totp_secret text,
    totp_enabled boolean NOT NULL DEFAULT false,
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

CREATE TABLE IF NOT EXISTS sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    csrf_hash bytea NOT NULL,
    remote_ip text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions(user_id);
CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    id uuid PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    label text NOT NULL DEFAULT '',
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS nodes (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'online', 'offline', 'revoked')),
    os text NOT NULL DEFAULT '',
    arch text NOT NULL DEFAULT '',
    agent_version text NOT NULL DEFAULT '',
    address text NOT NULL DEFAULT '',
    instance_root text NOT NULL DEFAULT '',
    agent_credential_hash bytea UNIQUE,
    cpu_percent double precision NOT NULL DEFAULT 0,
    memory_used bigint NOT NULL DEFAULT 0 CHECK (memory_used >= 0),
    memory_total bigint NOT NULL DEFAULT 0 CHECK (memory_total >= 0),
    disk_used bigint NOT NULL DEFAULT 0 CHECK (disk_used >= 0),
    disk_total bigint NOT NULL DEFAULT 0 CHECK (disk_total >= 0),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS nodes_status_idx ON nodes(status);

CREATE TABLE IF NOT EXISTS instances (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    name text NOT NULL,
    status text NOT NULL CHECK (status IN ('installing', 'stopped', 'starting', 'running', 'stopping', 'updating', 'degraded', 'external')),
    path text NOT NULL DEFAULT '',
    host text NOT NULL DEFAULT '0.0.0.0',
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    tv_port integer NOT NULL DEFAULT 0 CHECK (tv_port BETWEEN 0 AND 65535),
    pid integer NOT NULL DEFAULT 0,
    map text NOT NULL DEFAULT '',
    mode text NOT NULL DEFAULT '',
    players integer NOT NULL DEFAULT 0,
    max_players integer NOT NULL DEFAULT 0,
    build_id text NOT NULL DEFAULT '',
    version integer NOT NULL DEFAULT 1,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    started_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(node_id, port)
);
CREATE INDEX IF NOT EXISTS instances_node_id_idx ON instances(node_id);
CREATE INDEX IF NOT EXISTS instances_public_idx ON instances (((config->>'public_status')::boolean))
    WHERE config ? 'public_status';

CREATE TABLE IF NOT EXISTS instance_secrets (
    instance_id uuid PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
    rcon_ciphertext text NOT NULL,
    server_password_ciphertext text,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS jobs (
    id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    instance_id uuid REFERENCES instances(id) ON DELETE SET NULL,
    type text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'dispatched', 'running', 'succeeded', 'failed', 'canceled')),
    progress integer NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message text NOT NULL DEFAULT '',
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    result jsonb NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key text,
    requested_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);
CREATE INDEX IF NOT EXISTS jobs_node_status_idx ON jobs(node_id, status, created_at);
CREATE INDEX IF NOT EXISTS jobs_instance_idx ON jobs(instance_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_idempotency_idx ON jobs(requested_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE UNIQUE INDEX IF NOT EXISTS jobs_system_idempotency_idx ON jobs(idempotency_key)
    WHERE requested_by IS NULL AND idempotency_key IS NOT NULL AND idempotency_key <> '';
CREATE UNIQUE INDEX IF NOT EXISTS jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND status IN ('queued', 'dispatched', 'running')
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list');

CREATE TABLE IF NOT EXISTS metrics (
    id bigserial PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id uuid REFERENCES instances(id) ON DELETE CASCADE,
    cpu_percent double precision NOT NULL DEFAULT 0,
    memory_used bigint NOT NULL DEFAULT 0,
    memory_total bigint NOT NULL DEFAULT 0,
    disk_used bigint NOT NULL DEFAULT 0,
    disk_total bigint NOT NULL DEFAULT 0,
    network_rx bigint NOT NULL DEFAULT 0,
    network_tx bigint NOT NULL DEFAULT 0,
    players integer NOT NULL DEFAULT 0,
    collected_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS metrics_node_time_idx ON metrics(node_id, collected_at DESC);
CREATE INDEX IF NOT EXISTS metrics_instance_time_idx ON metrics(instance_id, collected_at DESC);

CREATE TABLE IF NOT EXISTS audit_events (
    id uuid PRIMARY KEY,
    actor_id uuid REFERENCES users(id) ON DELETE SET NULL,
    actor_name text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL DEFAULT '',
    remote_ip text NOT NULL DEFAULT '',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_created_idx ON audit_events(created_at DESC);

CREATE TABLE IF NOT EXISTS event_log (
    id bigserial PRIMARY KEY,
    type text NOT NULL,
    resource_type text NOT NULL DEFAULT '',
    resource_id text NOT NULL DEFAULT '',
    data jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS event_log_created_idx ON event_log(created_at DESC);

CREATE TABLE IF NOT EXISTS config_versions (
    id uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    version integer NOT NULL,
    config jsonb NOT NULL,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(instance_id, version)
);

CREATE TABLE IF NOT EXISTS addon_packages (
    id uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name text NOT NULL,
    kind text NOT NULL,
    version text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT '',
    source_id text NOT NULL DEFAULT '',
    sha256 text NOT NULL DEFAULT '',
    path text NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT true,
    risk text NOT NULL DEFAULT 'normal',
    installed_by uuid REFERENCES users(id) ON DELETE SET NULL,
    installed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS addon_packages_instance_idx ON addon_packages(instance_id,installed_at DESC);

CREATE TABLE IF NOT EXISTS backups (
    id uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name text NOT NULL,
    kind text NOT NULL DEFAULT 'manual',
    path text NOT NULL,
    platform text NOT NULL DEFAULT '',
    size bigint NOT NULL DEFAULT 0 CHECK(size>=0),
    sha256 text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS backups_instance_idx ON backups(instance_id,created_at DESC);

CREATE TABLE IF NOT EXISTS schedules (
    id uuid PRIMARY KEY,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name text NOT NULL,
    cron text NOT NULL,
    timezone text NOT NULL DEFAULT 'Asia/Hong_Kong',
    action text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    misfire_policy text NOT NULL DEFAULT 'skip',
    last_run_at timestamptz,
    next_run_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS schedules_instance_idx ON schedules(instance_id,created_at);

CREATE TABLE IF NOT EXISTS notifications (
    id uuid PRIMARY KEY,
    severity text NOT NULL CHECK(severity IN ('info','warning','critical')),
    type text NOT NULL,
    title text NOT NULL,
    message text NOT NULL,
    resource text NOT NULL DEFAULT '',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    read_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notifications_created_idx ON notifications(created_at DESC);
