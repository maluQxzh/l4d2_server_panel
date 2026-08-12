-- Backup deletion is a new destructive instance-scoped permission. Existing
-- global owner bindings are immutable presets and receive it automatically;
-- customized administrator bindings remain unchanged until an owner grants it.
INSERT OR IGNORE INTO role_binding_permissions(binding_id,permission)
SELECT id,'backups.delete'
FROM role_bindings
WHERE role='owner' AND scope_type='global';
