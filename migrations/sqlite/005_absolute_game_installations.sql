-- Absolute game installation paths replace the legacy root-index/relative-path
-- model. Legacy relative installations and their bound instances are removed
-- deliberately; this migration is an explicit compatibility break.

DELETE FROM instances
WHERE installation_id IN (
    SELECT installation.id
    FROM game_installations installation
    JOIN nodes node ON node.id=installation.node_id
    WHERE NOT (
        (lower(node.os) LIKE '%windows%' AND replace(installation.relative_path,'\','/') GLOB '[A-Za-z]:/*')
        OR (lower(node.os) NOT LIKE '%windows%' AND substr(installation.relative_path,1,1)='/')
    )
);

CREATE TABLE game_installations_absolute (
    id TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    path TEXT NOT NULL CHECK (length(path) BETWEEN 1 AND 1024),
    path_key TEXT NOT NULL CHECK (length(path_key) BETWEEN 1 AND 1024),
    app_id INTEGER NOT NULL DEFAULT 222860 CHECK (app_id=222860),
    status TEXT NOT NULL CHECK (status IN ('pending','installing','validating','updating','ready','degraded')),
    build_id TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    last_checked_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(environment_id,path_key),
    CHECK (environment_id='environment-' || node_id)
);

INSERT INTO game_installations_absolute(
    id,environment_id,node_id,path,path_key,app_id,status,build_id,last_error,last_checked_at,created_at,updated_at
)
SELECT installation.id,installation.environment_id,installation.node_id,
    replace(installation.relative_path,'\','/'),installation.path_key,installation.app_id,
    installation.status,installation.build_id,installation.last_error,installation.last_checked_at,
    installation.created_at,installation.updated_at
FROM game_installations installation
JOIN nodes node ON node.id=installation.node_id
WHERE installation.status<>'quarantined' AND (
    (lower(node.os) LIKE '%windows%' AND replace(installation.relative_path,'\','/') GLOB '[A-Za-z]:/*')
    OR (lower(node.os) NOT LIKE '%windows%' AND substr(installation.relative_path,1,1)='/')
);

-- Existing absolute rows remain as unbound installation records. Clearing
-- references before replacing the table keeps foreign keys valid throughout.
UPDATE instances SET installation_id=NULL WHERE installation_id IS NOT NULL;
UPDATE jobs SET game_installation_id=NULL WHERE game_installation_id IS NOT NULL;

DROP INDEX game_installations_node_idx;
DROP TABLE game_installations;
ALTER TABLE game_installations_absolute RENAME TO game_installations;

CREATE INDEX game_installations_node_idx ON game_installations(node_id,status,created_at);

DROP INDEX jobs_active_installation_write_idx;
CREATE UNIQUE INDEX jobs_active_installation_write_idx ON jobs(game_installation_id)
WHERE game_installation_id IS NOT NULL AND status IN ('queued','dispatched','running')
  AND type IN ('game.install','game.import','game.validate','game.update');
