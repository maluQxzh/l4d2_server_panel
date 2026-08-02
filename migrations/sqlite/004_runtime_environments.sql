CREATE TABLE environments (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL UNIQUE REFERENCES nodes(id) ON DELETE CASCADE,
    system_dependencies TEXT NOT NULL DEFAULT '{}' CHECK (typeof(system_dependencies)='text' AND json_valid(system_dependencies)),
    steamcmd_source TEXT NOT NULL DEFAULT 'unconfigured' CHECK (steamcmd_source IN ('unconfigured','managed','custom')),
    steamcmd_path TEXT NOT NULL DEFAULT '',
    steamcmd_status TEXT NOT NULL DEFAULT 'unconfigured' CHECK (steamcmd_status IN ('unconfigured','checking','ready','error')),
    steamcmd_version TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    last_inspected_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);

INSERT INTO environments(id,node_id)
SELECT 'environment-' || id,id FROM nodes;

CREATE TRIGGER nodes_create_environment AFTER INSERT ON nodes
BEGIN
    INSERT INTO environments(id,node_id) VALUES('environment-' || NEW.id,NEW.id);
END;

CREATE TABLE game_installations (
    id TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    root_index INTEGER NOT NULL CHECK (root_index>=0),
    relative_path TEXT NOT NULL CHECK (length(relative_path) BETWEEN 1 AND 1024),
    path_key TEXT NOT NULL CHECK (length(path_key) BETWEEN 1 AND 1024),
    app_id INTEGER NOT NULL DEFAULT 222860 CHECK (app_id=222860),
    status TEXT NOT NULL CHECK (status IN ('pending','installing','validating','updating','ready','degraded','quarantined')),
    build_id TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    last_checked_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    updated_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(environment_id,root_index,path_key),
    CHECK (environment_id='environment-' || node_id)
);
CREATE INDEX game_installations_node_idx ON game_installations(node_id,status,created_at);

ALTER TABLE instances ADD COLUMN installation_id TEXT REFERENCES game_installations(id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX instances_installation_unique_idx ON instances(installation_id) WHERE installation_id IS NOT NULL;

INSERT INTO game_installations(id,environment_id,node_id,root_index,relative_path,path_key,status,build_id,last_checked_at,created_at,updated_at)
SELECT 'installation-' || instance.id,'environment-' || instance.node_id,instance.node_id,0,
    CASE
      WHEN node.instance_root<>'' AND replace(instance.path,'\','/') LIKE rtrim(replace(node.instance_root,'\','/'),'/') || '/%'
      THEN ltrim(substr(replace(instance.path,'\','/'),length(rtrim(replace(node.instance_root,'\','/'),'/'))+1),'/')
      ELSE replace(instance.path,'\','/')
    END,
    CASE WHEN lower(node.os) LIKE '%windows%' THEN lower(
        CASE
          WHEN node.instance_root<>'' AND replace(instance.path,'\','/') LIKE rtrim(replace(node.instance_root,'\','/'),'/') || '/%'
          THEN ltrim(substr(replace(instance.path,'\','/'),length(rtrim(replace(node.instance_root,'\','/'),'/'))+1),'/')
          ELSE replace(instance.path,'\','/')
        END
    ) ELSE
        CASE
          WHEN node.instance_root<>'' AND replace(instance.path,'\','/') LIKE rtrim(replace(node.instance_root,'\','/'),'/') || '/%'
          THEN ltrim(substr(replace(instance.path,'\','/'),length(rtrim(replace(node.instance_root,'\','/'),'/'))+1),'/')
          ELSE replace(instance.path,'\','/')
        END
    END,
    CASE WHEN instance.status IN ('stopped','running','external') THEN 'ready' ELSE 'degraded' END,
    instance.build_id,instance.updated_at,instance.created_at,instance.updated_at
FROM instances instance JOIN nodes node ON node.id=instance.node_id
WHERE trim(instance.path)<>'';

UPDATE instances SET installation_id='installation-' || id WHERE trim(path)<>'';

ALTER TABLE jobs ADD COLUMN game_installation_id TEXT REFERENCES game_installations(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX jobs_active_installation_write_idx ON jobs(game_installation_id)
WHERE game_installation_id IS NOT NULL AND status IN ('queued','dispatched','running')
  AND type IN ('game.install','game.import','game.validate','game.update','game.reinstall','game.quarantine');
CREATE UNIQUE INDEX jobs_active_environment_steamcmd_idx ON jobs(node_id)
WHERE instance_id IS NULL AND status IN ('queued','dispatched','running')
  AND type IN ('steamcmd.configure','steamcmd.clear','steamcmd.install','steamcmd.repair');

UPDATE instances
SET config=json_remove(config,'$.maintenance_cron')
WHERE json_type(config,'$.maintenance_cron') IS NOT NULL;

UPDATE schedules SET enabled=0
WHERE action IN ('update','validate');

DELETE FROM instance_update_state;
