-- Fine-grained permissions that no longer correspond to a public operation
-- must not remain assignable. Delete bindings that would otherwise become
-- empty: older Control versions treated an empty binding as its role preset,
-- while current authorization deliberately treats explicit permissions as
-- authoritative and fails closed.
--
-- Migration 009 accidentally omitted packages.delete from its role presets.
-- Backfill it when a binding still contains every permission from its complete
-- 009 role/scope preset. Admin bindings must also predate migration 009 because
-- a later admin may deliberately omit packages.delete. Global owner bindings are
-- fixed presets and cannot be customized, so they are eligible regardless of
-- creation time. Both timestamps use Store's unix_micro integer representation.
WITH legacy_permission_presets(permission,min_rank) AS (VALUES
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
), legacy_expected_permissions(binding_id,permission) AS (
    SELECT binding.id,preset.permission
    FROM role_bindings binding
    JOIN legacy_permission_presets preset
      ON preset.min_rank<=CASE binding.role WHEN 'owner' THEN 4 ELSE 3 END
    WHERE binding.role IN ('admin','owner') AND (
        binding.scope_type='global'
        OR (binding.scope_type='node' AND preset.permission NOT IN
            ('nodes.enroll','audit.read_global','notifications.manage','root_security'))
        OR (binding.scope_type='instance' AND (
            (preset.permission LIKE 'instances.%' AND preset.permission<>'instances.create')
            OR preset.permission LIKE 'players.%'
            OR preset.permission='rcon.execute'
            OR preset.permission LIKE 'files.%'
            OR preset.permission LIKE 'packages.%'
            OR preset.permission LIKE 'backups.%'
            OR preset.permission LIKE 'schedules.%'
            OR preset.permission='logs.read'
            OR preset.permission='maps.assign'
            OR preset.permission LIKE 'jobs.%'
            OR preset.permission IN ('audit.read','notifications.read','metrics.read')
        ))
    )
), complete_legacy_bindings(binding_id) AS (
    SELECT binding.id
    FROM role_bindings binding
    WHERE binding.role IN ('admin','owner')
      AND (
          (binding.role='owner' AND binding.scope_type='global')
          OR (
              binding.role='admin'
              AND binding.created_at < (
                  SELECT applied_at
                  FROM schema_migrations
                  WHERE version='009_fine_grained_permissions.sql'
              )
          )
      )
      AND NOT EXISTS (
          SELECT 1
          FROM legacy_expected_permissions expected
          WHERE expected.binding_id=binding.id
            AND NOT EXISTS (
                SELECT 1
                FROM role_binding_permissions granted
                WHERE granted.binding_id=binding.id
                  AND granted.permission=expected.permission
            )
      )
)
INSERT OR IGNORE INTO role_binding_permissions(binding_id,permission)
SELECT binding_id,'packages.delete' FROM complete_legacy_bindings;

DELETE FROM role_bindings
WHERE NOT EXISTS (
    SELECT 1
    FROM role_binding_permissions permission
    WHERE permission.binding_id=role_bindings.id
      AND permission.permission NOT IN ('instances.update','instances.validate','files.upload')
);

DELETE FROM role_binding_permissions
WHERE permission IN ('instances.update','instances.validate','files.upload');

-- Instance-level game maintenance schedules were removed in favor of explicit
-- runtime-environment operations. Preserve their history, but make sure an
-- upgraded Control never keeps attempting to execute them.
UPDATE schedules
SET enabled=0
WHERE enabled=1 AND action IN ('update','validate');
