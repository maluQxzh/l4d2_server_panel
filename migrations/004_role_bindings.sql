ALTER TABLE users ADD COLUMN IF NOT EXISTS is_bootstrap_owner boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS role_bindings (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'operator', 'viewer')),
    scope_type text NOT NULL CHECK (scope_type IN ('global', 'node', 'instance')),
    node_id uuid REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id uuid REFERENCES instances(id) ON DELETE CASCADE,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (scope_type='global' AND node_id IS NULL AND instance_id IS NULL) OR
        (scope_type='node' AND node_id IS NOT NULL AND instance_id IS NULL) OR
        (scope_type='instance' AND node_id IS NULL AND instance_id IS NOT NULL)
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS role_bindings_unique_scope_idx
    ON role_bindings(user_id,scope_type,node_id,instance_id) NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS role_bindings_user_idx ON role_bindings(user_id);
CREATE INDEX IF NOT EXISTS role_bindings_node_idx ON role_bindings(node_id) WHERE node_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS role_bindings_instance_idx ON role_bindings(instance_id) WHERE instance_id IS NOT NULL;

-- Preserve the original users.role semantics for every deployment upgraded
-- from the single-global-role model.
INSERT INTO role_bindings(id,user_id,role,scope_type)
SELECT gen_random_uuid(),u.id,u.role,'global' FROM users u
WHERE NOT EXISTS (SELECT 1 FROM role_bindings rb WHERE rb.user_id=u.id);

-- The first owner is the recovery/root account. Application transactions
-- additionally guarantee that at least one enabled global owner remains.
UPDATE users SET is_bootstrap_owner=true WHERE id=(
    SELECT id FROM users WHERE role='owner' ORDER BY created_at,id LIMIT 1
) AND NOT EXISTS (SELECT 1 FROM users WHERE is_bootstrap_owner);

CREATE OR REPLACE FUNCTION sync_role_bindings_legacy_role() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    affected_user uuid;
BEGIN
    affected_user := COALESCE(NEW.user_id, OLD.user_id);
    UPDATE users SET role=COALESCE((
        SELECT rb.role FROM role_bindings rb WHERE rb.user_id=affected_user
        ORDER BY CASE rb.role WHEN 'owner' THEN 4 WHEN 'admin' THEN 3 WHEN 'operator' THEN 2 ELSE 1 END DESC
        LIMIT 1
    ), 'viewer') WHERE id=affected_user;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS role_bindings_sync_legacy_role ON role_bindings;
CREATE TRIGGER role_bindings_sync_legacy_role
AFTER INSERT OR UPDATE OR DELETE ON role_bindings
FOR EACH ROW EXECUTE FUNCTION sync_role_bindings_legacy_role();
