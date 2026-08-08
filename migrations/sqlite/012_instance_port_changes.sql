CREATE TABLE pending_instance_port_changes (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL UNIQUE REFERENCES instances(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER)),
    UNIQUE(node_id,port)
);

CREATE TRIGGER pending_instance_port_not_reserved
BEFORE INSERT ON pending_instance_port_changes
WHEN EXISTS (
    SELECT 1 FROM instance_port_reservations
    WHERE node_id=NEW.node_id AND port=NEW.port
)
BEGIN
    SELECT RAISE(ABORT, 'instance port is already reserved');
END;

CREATE TRIGGER reserved_instance_port_not_pending_insert
BEFORE INSERT ON instance_port_reservations
WHEN EXISTS (
    SELECT 1 FROM pending_instance_port_changes
    WHERE node_id=NEW.node_id AND port=NEW.port
)
BEGIN
    SELECT RAISE(ABORT, 'instance port change is pending');
END;

CREATE TRIGGER reserved_instance_port_not_pending_update
BEFORE UPDATE OF node_id,port ON instance_port_reservations
WHEN EXISTS (
    SELECT 1 FROM pending_instance_port_changes
    WHERE node_id=NEW.node_id AND port=NEW.port
)
BEGIN
    SELECT RAISE(ABORT, 'instance port change is pending');
END;
