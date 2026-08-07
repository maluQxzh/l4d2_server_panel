CREATE TABLE role_binding_permissions (
    binding_id TEXT NOT NULL REFERENCES role_bindings(id) ON DELETE CASCADE,
    permission TEXT NOT NULL,
    PRIMARY KEY (binding_id,permission)
);

CREATE INDEX role_binding_permissions_permission_idx
    ON role_binding_permissions(permission,binding_id);

WITH permission_presets(permission,min_rank) AS (VALUES
    ('nodes.read',1),
    ('nodes.enroll',3),
    ('nodes.update',3),
    ('nodes.credentials.rotate',3),
    ('nodes.discover',3),
    ('nodes.quarantine.read',3),
    ('nodes.quarantine.purge',3),
    ('nodes.delete',3),
    ('environments.read',1),
    ('environments.manage',3),
    ('instances.read',1),
    ('instances.create',3),
    ('instances.configure',2),
    ('instances.delete',3),
    ('instances.start',2),
    ('instances.stop',2),
    ('instances.restart',2),
    ('instances.update',2),
    ('instances.validate',2),
    ('players.read',1),
    ('players.kick',2),
    ('players.ban',2),
    ('players.unban',2),
    ('players.change_map',2),
    ('players.broadcast',2),
    ('rcon.execute',2),
    ('files.list',1),
    ('files.read',2),
    ('files.write',2),
    ('files.delete',2),
    ('files.upload',3),
    ('packages.read',1),
    ('packages.install',3),
    ('packages.toggle',3),
    ('packages.rollback',3),
    ('backups.read',1),
    ('backups.download',2),
    ('backups.create',2),
    ('backups.restore',2),
    ('schedules.read',1),
    ('schedules.manage',2),
    ('logs.read',1),
    ('maps.read',1),
    ('maps.import',3),
    ('maps.assign',3),
    ('maps.verify',3),
    ('maps.delete',3),
    ('jobs.read',1),
    ('jobs.details',2),
    ('jobs.cancel',2),
    ('audit.read',1),
    ('audit.read_global',3),
    ('notifications.read',1),
    ('notifications.manage',3),
    ('metrics.read',1),
    ('root_security',4)
)
INSERT INTO role_binding_permissions(binding_id,permission)
SELECT rb.id,p.permission
FROM role_bindings rb
JOIN permission_presets p ON p.min_rank<=CASE rb.role
    WHEN 'owner' THEN 4
    WHEN 'admin' THEN 3
    WHEN 'operator' THEN 2
    ELSE 1
END
WHERE rb.scope_type='global'
   OR (rb.scope_type='node' AND p.permission NOT IN ('nodes.enroll','audit.read_global','notifications.manage','root_security'))
   OR (rb.scope_type='instance' AND (
        (p.permission LIKE 'instances.%' AND p.permission<>'instances.create')
        OR p.permission LIKE 'players.%'
        OR p.permission='rcon.execute'
        OR p.permission LIKE 'files.%'
        OR p.permission LIKE 'packages.%'
        OR p.permission LIKE 'backups.%'
        OR p.permission LIKE 'schedules.%'
        OR p.permission='logs.read'
        OR p.permission='maps.assign'
        OR p.permission LIKE 'jobs.%'
        OR p.permission IN ('audit.read','notifications.read','metrics.read')
   ));
