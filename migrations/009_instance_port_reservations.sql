CREATE TABLE instance_port_reservations (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    kind text NOT NULL CHECK (kind IN ('game', 'source_tv')),
    PRIMARY KEY (node_id, port),
    UNIQUE (instance_id, kind)
);
CREATE INDEX instance_port_reservations_instance_idx
    ON instance_port_reservations(instance_id);

-- Refuse to silently reinterpret an existing conflicting layout. Operators can
-- move one of the ports and retry the migration without losing any data.
DO $$
DECLARE
    conflict_node uuid;
    conflict_port integer;
BEGIN
    SELECT occupied.node_id, occupied.port
      INTO conflict_node, conflict_port
      FROM (
          SELECT node_id, port FROM instances
          UNION ALL
          SELECT node_id, tv_port AS port FROM instances WHERE tv_port <> 0
      ) AS occupied
     GROUP BY occupied.node_id, occupied.port
    HAVING count(*) > 1
     ORDER BY occupied.node_id, occupied.port
     LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION 'existing instance port collision on node %, port %', conflict_node, conflict_port
            USING ERRCODE = 'unique_violation';
    END IF;
END
$$;

INSERT INTO instance_port_reservations(node_id, instance_id, port, kind)
SELECT node_id, id, port, 'game' FROM instances;

INSERT INTO instance_port_reservations(node_id, instance_id, port, kind)
SELECT node_id, id, tv_port, 'source_tv' FROM instances WHERE tv_port <> 0;

CREATE OR REPLACE FUNCTION sync_instance_port_reservations()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        DELETE FROM instance_port_reservations WHERE instance_id = NEW.id;
    END IF;

    INSERT INTO instance_port_reservations(node_id, instance_id, port, kind)
    VALUES (NEW.node_id, NEW.id, NEW.port, 'game');

    IF NEW.tv_port <> 0 THEN
        INSERT INTO instance_port_reservations(node_id, instance_id, port, kind)
        VALUES (NEW.node_id, NEW.id, NEW.tv_port, 'source_tv');
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER instances_sync_port_reservations
AFTER INSERT OR UPDATE OF node_id, port, tv_port ON instances
FOR EACH ROW EXECUTE FUNCTION sync_instance_port_reservations();
