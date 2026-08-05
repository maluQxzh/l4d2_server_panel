CREATE TABLE maps (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    health TEXT NOT NULL DEFAULT 'healthy' CHECK (health IN ('healthy','degraded','broken','deleting')),
    aggregate_sha256 TEXT NOT NULL CHECK (length(aggregate_sha256)=64 AND aggregate_sha256 NOT GLOB '*[^0-9a-f]*'),
    total_size INTEGER NOT NULL DEFAULT 0 CHECK (total_size>=0),
    source_type TEXT NOT NULL DEFAULT 'qq_flash' CHECK (source_type IN ('qq_flash')),
    source_name TEXT NOT NULL DEFAULT '',
    removed_files INTEGER NOT NULL DEFAULT 0 CHECK (removed_files>=0),
    removed_bytes INTEGER NOT NULL DEFAULT 0 CHECK (removed_bytes>=0),
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX maps_node_created_idx ON maps(node_id,created_at DESC,id DESC);
CREATE INDEX maps_node_hash_idx ON maps(node_id,aggregate_sha256);

CREATE TABLE map_artifacts (
    id TEXT PRIMARY KEY,
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal>=0),
    sha256 TEXT NOT NULL CHECK (length(sha256)=64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    size INTEGER NOT NULL CHECK (size>0),
    UNIQUE(map_id,ordinal)
);
CREATE INDEX map_artifacts_map_idx ON map_artifacts(map_id,ordinal);

CREATE TABLE map_bsps (
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    virtual_path TEXT NOT NULL,
    start_name TEXT NOT NULL DEFAULT '',
    dictionary_present INTEGER NOT NULL CHECK (dictionary_present IN (0,1)),
    PRIMARY KEY(map_id,virtual_path)
);

CREATE TABLE map_warnings (
    id TEXT PRIMARY KEY,
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    code TEXT NOT NULL,
    message TEXT NOT NULL,
    virtual_path TEXT NOT NULL DEFAULT ''
);
CREATE INDEX map_warnings_map_idx ON map_warnings(map_id,id);

CREATE TABLE map_replicas (
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    filesystem_id TEXT NOT NULL,
    health TEXT NOT NULL CHECK (health IN ('healthy','missing','corrupt')),
    verified_at TIMESTAMP,
    PRIMARY KEY(map_id,filesystem_id)
);

CREATE TABLE map_assignments (
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    restart_required INTEGER NOT NULL DEFAULT 0 CHECK (restart_required IN (0,1)),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    PRIMARY KEY(map_id,instance_id)
);
CREATE INDEX map_assignments_instance_idx ON map_assignments(instance_id,map_id);

CREATE TABLE map_delete_confirmations (
    id TEXT PRIMARY KEY,
    map_id TEXT NOT NULL REFERENCES maps(id) ON DELETE CASCADE,
    requested_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash)=32),
    expires_at TIMESTAMP NOT NULL,
    consumed_at TIMESTAMP,
    job_id TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
CREATE INDEX map_delete_confirmations_expiry_idx ON map_delete_confirmations(expires_at) WHERE consumed_at IS NULL;

CREATE UNIQUE INDEX jobs_map_import_active_idx ON jobs(node_id)
    WHERE instance_id IS NULL AND type='map.import.qq_flash' AND status IN ('queued','dispatched','running');
