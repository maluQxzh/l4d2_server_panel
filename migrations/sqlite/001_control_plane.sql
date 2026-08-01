CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    username_normalized TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('owner','admin','operator','viewer')),
    totp_secret TEXT,
    totp_enabled INTEGER NOT NULL DEFAULT 0 CHECK (totp_enabled IN (0,1)),
    disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0,1)),
    is_bootstrap_owner INTEGER NOT NULL DEFAULT 0 CHECK (is_bootstrap_owner IN (0,1)),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    last_login_at TIMESTAMP
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash)=32),
    csrf_hash BLOB NOT NULL CHECK (length(csrf_hash)=32),
    remote_ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    last_seen_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE enrollment_tokens (
    id TEXT PRIMARY KEY,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash)=32),
    label TEXT NOT NULL DEFAULT '',
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    expires_at TIMESTAMP NOT NULL,
    consumed_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);

CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','online','offline','revoked')),
    os TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL DEFAULT '',
    agent_version TEXT NOT NULL DEFAULT '',
    address TEXT NOT NULL DEFAULT '',
    public_address TEXT NOT NULL DEFAULT '',
    instance_root TEXT NOT NULL DEFAULT '',
    agent_credential_hash BLOB UNIQUE,
    cpu_percent REAL NOT NULL DEFAULT 0,
    memory_used INTEGER NOT NULL DEFAULT 0 CHECK (memory_used>=0),
    memory_total INTEGER NOT NULL DEFAULT 0 CHECK (memory_total>=0),
    disk_used INTEGER NOT NULL DEFAULT 0 CHECK (disk_used>=0),
    disk_total INTEGER NOT NULL DEFAULT 0 CHECK (disk_total>=0),
    last_seen_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX nodes_status_idx ON nodes(status);

CREATE TABLE instances (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('installing','stopped','starting','running','stopping','updating','degraded','external')),
    path TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL DEFAULT '0.0.0.0',
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    tv_port INTEGER NOT NULL DEFAULT 0 CHECK (tv_port BETWEEN 0 AND 65535),
    pid INTEGER NOT NULL DEFAULT 0,
    map TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL DEFAULT '',
    players INTEGER NOT NULL DEFAULT 0,
    max_players INTEGER NOT NULL DEFAULT 0,
    build_id TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 1,
    config TEXT NOT NULL DEFAULT '{}' CHECK (typeof(config)='text' AND json_valid(config)),
    started_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(node_id,port)
);
CREATE INDEX instances_node_id_idx ON instances(node_id);
CREATE INDEX instances_public_idx ON instances(CAST(json_extract(config,'$.public_status') AS INTEGER))
    WHERE json_type(config,'$.public_status') IS NOT NULL;

CREATE TABLE instance_secrets (
    instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
    rcon_ciphertext TEXT NOT NULL,
    server_password_ciphertext TEXT,
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    instance_id TEXT REFERENCES instances(id) ON DELETE SET NULL,
    type TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued','dispatched','running','succeeded','failed','canceled')),
    progress INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message TEXT NOT NULL DEFAULT '',
    payload TEXT NOT NULL DEFAULT '{}' CHECK (typeof(payload)='text' AND json_valid(payload)),
    result TEXT NOT NULL DEFAULT '{}' CHECK (typeof(result)='text' AND json_valid(result)),
    idempotency_key TEXT,
    request_fingerprint TEXT CHECK (request_fingerprint IS NULL OR (length(request_fingerprint)=64 AND request_fingerprint NOT GLOB '*[^0-9a-f]*')),
    requested_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    started_at TIMESTAMP,
    finished_at TIMESTAMP,
    is_instance_write INTEGER GENERATED ALWAYS AS (
        CASE WHEN instance_id IS NOT NULL
          AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort')
          AND NOT (type='file' AND json_extract(payload,'$.operation')='read')
        THEN 1 ELSE 0 END
    ) STORED,
    holds_instance_write_lock INTEGER GENERATED ALWAYS AS (
        CASE WHEN instance_id IS NOT NULL
          AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort')
          AND NOT (type='file' AND json_extract(payload,'$.operation')='read')
          AND (status IN ('queued','dispatched','running')
            OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1)
            OR (status IN ('succeeded','failed') AND json_extract(result,'$._projection_pending')=1))
        THEN 1 ELSE 0 END
    ) STORED
);
CREATE INDEX jobs_node_status_idx ON jobs(node_id,status,created_at);
CREATE INDEX jobs_instance_idx ON jobs(instance_id,created_at DESC);
CREATE UNIQUE INDEX jobs_idempotency_idx ON jobs(requested_by,idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key<>'';
CREATE UNIQUE INDEX jobs_system_idempotency_idx ON jobs(idempotency_key)
    WHERE requested_by IS NULL AND idempotency_key IS NOT NULL AND idempotency_key<>'';
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id) WHERE holds_instance_write_lock=1;
CREATE UNIQUE INDEX jobs_read_probe_active_idx ON jobs(instance_id,type)
    WHERE instance_id IS NOT NULL AND status IN ('queued','dispatched','running')
      AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update');
CREATE INDEX jobs_backup_upload_active_idx ON jobs(json_extract(payload,'$.upload_id'))
    WHERE type='backup.upload.chunk' AND status IN ('queued','dispatched','running','succeeded');

CREATE TABLE metrics (
    id INTEGER PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT REFERENCES instances(id) ON DELETE CASCADE,
    cpu_percent REAL NOT NULL DEFAULT 0,
    memory_used INTEGER NOT NULL DEFAULT 0,
    memory_total INTEGER NOT NULL DEFAULT 0,
    disk_used INTEGER NOT NULL DEFAULT 0,
    disk_total INTEGER NOT NULL DEFAULT 0,
    network_rx INTEGER NOT NULL DEFAULT 0,
    network_tx INTEGER NOT NULL DEFAULT 0,
    players INTEGER NOT NULL DEFAULT 0,
    collected_at TIMESTAMP NOT NULL
);
CREATE INDEX metrics_node_time_idx ON metrics(node_id,collected_at DESC);
CREATE INDEX metrics_instance_time_idx ON metrics(instance_id,collected_at DESC);

CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    actor_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    actor_name TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL DEFAULT '',
    remote_ip TEXT NOT NULL DEFAULT '',
    metadata TEXT NOT NULL DEFAULT '{}' CHECK (typeof(metadata)='text' AND json_valid(metadata)),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX audit_events_created_idx ON audit_events(created_at DESC);

CREATE TABLE event_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL,
    resource_type TEXT NOT NULL DEFAULT '',
    resource_id TEXT NOT NULL DEFAULT '',
    data TEXT NOT NULL DEFAULT '{}' CHECK (typeof(data)='text' AND json_valid(data)),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX event_log_created_idx ON event_log(created_at DESC);

CREATE TABLE config_versions (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    config TEXT NOT NULL CHECK (typeof(config)='text' AND json_valid(config)),
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    source_job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    UNIQUE(instance_id,version),
    UNIQUE(source_job_id)
);

CREATE TABLE addon_packages (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    source_id TEXT NOT NULL DEFAULT '',
    sha256 TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    risk TEXT NOT NULL DEFAULT 'normal',
    installed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    installed_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    last_job_id TEXT
);
CREATE INDEX addon_packages_instance_idx ON addon_packages(instance_id,installed_at DESC);

CREATE TABLE addon_package_versions (
    id TEXT PRIMARY KEY,
    package_id TEXT NOT NULL REFERENCES addon_packages(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    source_job_id TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    sha256 TEXT NOT NULL CHECK (length(sha256)=64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    size INTEGER NOT NULL DEFAULT 0 CHECK (size>=0),
    storage_path TEXT NOT NULL,
    target_path TEXT NOT NULL,
    manifest TEXT NOT NULL DEFAULT '{"files":[]}' CHECK (typeof(manifest)='text' AND json_valid(manifest)),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(source_job_id,storage_path)
);
CREATE INDEX addon_package_versions_package_idx ON addon_package_versions(package_id,created_at DESC,id);
CREATE INDEX addon_package_versions_instance_idx ON addon_package_versions(instance_id,created_at DESC,id);

CREATE TABLE backups (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'manual',
    path TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0 CHECK (size>=0),
    sha256 TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX backups_instance_idx ON backups(instance_id,created_at DESC);

CREATE TABLE schedules (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    cron TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'Asia/Hong_Kong',
    action TEXT NOT NULL,
    payload TEXT NOT NULL DEFAULT '{}' CHECK (typeof(payload)='text' AND json_valid(payload)),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    misfire_policy TEXT NOT NULL DEFAULT 'skip',
    last_run_at TIMESTAMP,
    next_run_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX schedules_instance_idx ON schedules(instance_id,created_at);

CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    severity TEXT NOT NULL CHECK (severity IN ('info','warning','critical')),
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    resource TEXT NOT NULL DEFAULT '',
    metadata TEXT NOT NULL DEFAULT '{}' CHECK (typeof(metadata)='text' AND json_valid(metadata)),
    read_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX notifications_created_idx ON notifications(created_at DESC);

CREATE TABLE api_tokens (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash)=32),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    scope_mask INTEGER NOT NULL CHECK (scope_mask BETWEEN 1 AND 7),
    node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT REFERENCES instances(id) ON DELETE CASCADE,
    expires_at TIMESTAMP NOT NULL,
    revoked_at TIMESTAMP,
    last_used_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX api_tokens_user_created_idx ON api_tokens(user_id,created_at DESC);
CREATE INDEX api_tokens_expires_idx ON api_tokens(expires_at) WHERE revoked_at IS NULL;

CREATE TABLE totp_recovery_codes (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash BLOB NOT NULL CHECK (length(code_hash)=32),
    used_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(user_id,code_hash)
);
CREATE INDEX totp_recovery_codes_user_unused_idx ON totp_recovery_codes(user_id) WHERE used_at IS NULL;

CREATE TABLE metrics_1m (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT REFERENCES instances(id) ON DELETE CASCADE,
    bucket TIMESTAMP NOT NULL,
    cpu_percent REAL NOT NULL DEFAULT 0,
    memory_used INTEGER NOT NULL DEFAULT 0 CHECK (memory_used>=0),
    memory_total INTEGER NOT NULL DEFAULT 0 CHECK (memory_total>=0),
    disk_used INTEGER NOT NULL DEFAULT 0 CHECK (disk_used>=0),
    disk_total INTEGER NOT NULL DEFAULT 0 CHECK (disk_total>=0),
    network_rx INTEGER NOT NULL DEFAULT 0 CHECK (network_rx>=0),
    network_tx INTEGER NOT NULL DEFAULT 0 CHECK (network_tx>=0),
    players INTEGER NOT NULL DEFAULT 0 CHECK (players>=0),
    sample_count INTEGER NOT NULL DEFAULT 0 CHECK (sample_count>=0)
);
CREATE UNIQUE INDEX metrics_1m_scope_bucket_key ON metrics_1m(node_id,COALESCE(instance_id,''),bucket);
CREATE INDEX metrics_1m_node_time_idx ON metrics_1m(node_id,bucket DESC);
CREATE INDEX metrics_1m_instance_time_idx ON metrics_1m(instance_id,bucket DESC) WHERE instance_id IS NOT NULL;

CREATE TABLE metrics_1h (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT REFERENCES instances(id) ON DELETE CASCADE,
    bucket TIMESTAMP NOT NULL,
    cpu_percent REAL NOT NULL DEFAULT 0,
    memory_used INTEGER NOT NULL DEFAULT 0 CHECK (memory_used>=0),
    memory_total INTEGER NOT NULL DEFAULT 0 CHECK (memory_total>=0),
    disk_used INTEGER NOT NULL DEFAULT 0 CHECK (disk_used>=0),
    disk_total INTEGER NOT NULL DEFAULT 0 CHECK (disk_total>=0),
    network_rx INTEGER NOT NULL DEFAULT 0 CHECK (network_rx>=0),
    network_tx INTEGER NOT NULL DEFAULT 0 CHECK (network_tx>=0),
    players INTEGER NOT NULL DEFAULT 0 CHECK (players>=0),
    sample_count INTEGER NOT NULL DEFAULT 0 CHECK (sample_count>=0)
);
CREATE UNIQUE INDEX metrics_1h_scope_bucket_key ON metrics_1h(node_id,COALESCE(instance_id,''),bucket);
CREATE INDEX metrics_1h_node_time_idx ON metrics_1h(node_id,bucket DESC);
CREATE INDEX metrics_1h_instance_time_idx ON metrics_1h(instance_id,bucket DESC) WHERE instance_id IS NOT NULL;

CREATE TABLE notification_channels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('webhook','smtp')),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    config TEXT NOT NULL DEFAULT '{}' CHECK (typeof(config)='text' AND json_valid(config)),
    secret_ciphertext TEXT NOT NULL DEFAULT '',
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    deleted_at TIMESTAMP
);
CREATE UNIQUE INDEX notification_channels_name_idx ON notification_channels(lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE notification_deliveries (
    id TEXT PRIMARY KEY,
    notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE RESTRICT,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivering','succeeded','failed','skipped')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count>=0),
    next_attempt_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    last_error TEXT NOT NULL DEFAULT '',
    response_code INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    delivered_at TIMESTAMP,
    UNIQUE(notification_id,channel_id)
);
CREATE INDEX notification_deliveries_pending_idx ON notification_deliveries(next_attempt_at,id) WHERE status='pending';

CREATE TABLE notification_delivery_attempts (
    id INTEGER PRIMARY KEY,
    delivery_id TEXT NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt>0),
    succeeded INTEGER NOT NULL CHECK (succeeded IN (0,1)),
    response_code INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    attempted_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(delivery_id,attempt)
);
CREATE INDEX notification_delivery_attempts_delivery_idx ON notification_delivery_attempts(delivery_id,attempt DESC);

CREATE TABLE instance_update_state (
    instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
    installed_build TEXT NOT NULL DEFAULT '',
    available_build TEXT NOT NULL DEFAULT '',
    available INTEGER NOT NULL DEFAULT 0 CHECK (available IN (0,1)),
    checked_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);

CREATE TABLE role_bindings (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('owner','admin','operator','viewer')),
    scope_type TEXT NOT NULL CHECK (scope_type IN ('global','node','instance')),
    node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT REFERENCES instances(id) ON DELETE CASCADE,
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    CHECK ((scope_type='global' AND node_id IS NULL AND instance_id IS NULL)
        OR (scope_type='node' AND node_id IS NOT NULL AND instance_id IS NULL)
        OR (scope_type='instance' AND node_id IS NULL AND instance_id IS NOT NULL))
);
CREATE UNIQUE INDEX role_bindings_unique_scope_idx
    ON role_bindings(user_id,scope_type,COALESCE(node_id,''),COALESCE(instance_id,''));
CREATE INDEX role_bindings_user_idx ON role_bindings(user_id);
CREATE INDEX role_bindings_node_idx ON role_bindings(node_id) WHERE node_id IS NOT NULL;
CREATE INDEX role_bindings_instance_idx ON role_bindings(instance_id) WHERE instance_id IS NOT NULL;

CREATE TABLE player_bans (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    steam_id TEXT NOT NULL,
    ip_address TEXT,
    reason TEXT NOT NULL DEFAULT '',
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    expires_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(instance_id,steam_id)
);
CREATE INDEX player_bans_instance_expiry_idx ON player_bans(instance_id,expires_at);

CREATE TABLE instance_port_reservations (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    kind TEXT NOT NULL CHECK (kind IN ('game','source_tv')),
    PRIMARY KEY(node_id,port),
    UNIQUE(instance_id,kind)
);
CREATE INDEX instance_port_reservations_instance_idx ON instance_port_reservations(instance_id);

CREATE TABLE quarantine_entries (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    source_job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id) ON DELETE RESTRICT,
    original_instance_id TEXT NOT NULL,
    instance_name TEXT NOT NULL CHECK (length(instance_name) BETWEEN 1 AND 200),
    root_index INTEGER NOT NULL CHECK (root_index>=0),
    entry_name TEXT NOT NULL CHECK (length(entry_name) BETWEEN 18 AND 200 AND entry_name NOT GLOB '*[^A-Za-z0-9._-]*'),
    quarantined_at TIMESTAMP NOT NULL,
    retention_expires_at TIMESTAMP NOT NULL,
    purge_job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    purged_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(node_id,root_index,entry_name),
    CHECK (retention_expires_at=quarantined_at+604800000000),
    CHECK (purged_at IS NULL OR purged_at>=quarantined_at)
);
CREATE INDEX quarantine_entries_node_active_idx ON quarantine_entries(node_id,quarantined_at DESC) WHERE purged_at IS NULL;

CREATE TABLE quarantine_purge_confirmations (
    id TEXT PRIMARY KEY,
    quarantine_entry_id TEXT NOT NULL REFERENCES quarantine_entries(id) ON DELETE CASCADE,
    requested_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash)=32),
    expires_at TIMESTAMP NOT NULL,
    consumed_at TIMESTAMP,
    job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    CHECK (expires_at>created_at),
    CHECK (consumed_at IS NULL OR consumed_at>=created_at)
);
CREATE INDEX quarantine_purge_confirmations_expiry_idx ON quarantine_purge_confirmations(expires_at) WHERE consumed_at IS NULL;

CREATE TABLE backup_uploads (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 180),
    platform TEXT NOT NULL CHECK (platform IN ('windows/amd64','linux/amd64')),
    size INTEGER NOT NULL CHECK (size>0 AND size<=21474836480),
    sha256 TEXT NOT NULL CHECK (length(sha256)=64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    received_size INTEGER NOT NULL DEFAULT 0 CHECK (received_size>=0 AND received_size<=size),
    status TEXT NOT NULL DEFAULT 'uploading' CHECK (status IN ('uploading','ready','restoring','succeeded','failed','expired')),
    requested_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    create_idempotency_key TEXT,
    last_chunk_job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    restore_job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(instance_id,id)
);
CREATE INDEX backup_uploads_instance_created_idx ON backup_uploads(instance_id,created_at DESC);
CREATE INDEX backup_uploads_expiry_idx ON backup_uploads(expires_at) WHERE status IN ('uploading','ready');
CREATE UNIQUE INDEX backup_uploads_create_idempotency_idx ON backup_uploads(requested_by,create_idempotency_key)
    WHERE requested_by IS NOT NULL AND create_idempotency_key IS NOT NULL AND create_idempotency_key<>'';
