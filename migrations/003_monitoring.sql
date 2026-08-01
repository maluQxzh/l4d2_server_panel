-- Time-series rollups, notification outbox, and safe public addresses.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS public_address text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS metrics_1m (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id uuid REFERENCES instances(id) ON DELETE CASCADE,
    bucket timestamptz NOT NULL,
    cpu_percent double precision NOT NULL DEFAULT 0,
    memory_used bigint NOT NULL DEFAULT 0 CHECK (memory_used >= 0),
    memory_total bigint NOT NULL DEFAULT 0 CHECK (memory_total >= 0),
    disk_used bigint NOT NULL DEFAULT 0 CHECK (disk_used >= 0),
    disk_total bigint NOT NULL DEFAULT 0 CHECK (disk_total >= 0),
    network_rx bigint NOT NULL DEFAULT 0 CHECK (network_rx >= 0),
    network_tx bigint NOT NULL DEFAULT 0 CHECK (network_tx >= 0),
    players integer NOT NULL DEFAULT 0 CHECK (players >= 0),
    sample_count integer NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
    CONSTRAINT metrics_1m_scope_bucket_key UNIQUE NULLS NOT DISTINCT (node_id, instance_id, bucket)
);
CREATE INDEX IF NOT EXISTS metrics_1m_node_time_idx ON metrics_1m(node_id,bucket DESC);
CREATE INDEX IF NOT EXISTS metrics_1m_instance_time_idx ON metrics_1m(instance_id,bucket DESC) WHERE instance_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS metrics_1h (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_id uuid REFERENCES instances(id) ON DELETE CASCADE,
    bucket timestamptz NOT NULL,
    cpu_percent double precision NOT NULL DEFAULT 0,
    memory_used bigint NOT NULL DEFAULT 0 CHECK (memory_used >= 0),
    memory_total bigint NOT NULL DEFAULT 0 CHECK (memory_total >= 0),
    disk_used bigint NOT NULL DEFAULT 0 CHECK (disk_used >= 0),
    disk_total bigint NOT NULL DEFAULT 0 CHECK (disk_total >= 0),
    network_rx bigint NOT NULL DEFAULT 0 CHECK (network_rx >= 0),
    network_tx bigint NOT NULL DEFAULT 0 CHECK (network_tx >= 0),
    players integer NOT NULL DEFAULT 0 CHECK (players >= 0),
    sample_count integer NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
    CONSTRAINT metrics_1h_scope_bucket_key UNIQUE NULLS NOT DISTINCT (node_id, instance_id, bucket)
);
CREATE INDEX IF NOT EXISTS metrics_1h_node_time_idx ON metrics_1h(node_id,bucket DESC);
CREATE INDEX IF NOT EXISTS metrics_1h_instance_time_idx ON metrics_1h(instance_id,bucket DESC) WHERE instance_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS notification_channels (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('webhook','smtp')),
    enabled boolean NOT NULL DEFAULT true,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    secret_ciphertext text NOT NULL DEFAULT '',
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS notification_channels_name_idx
    ON notification_channels(lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS notification_deliveries (
    id uuid PRIMARY KEY,
    notification_id uuid NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    channel_id uuid NOT NULL REFERENCES notification_channels(id) ON DELETE RESTRICT,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivering','succeeded','failed','skipped')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text NOT NULL DEFAULT '',
    response_code integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    UNIQUE(notification_id,channel_id)
);
CREATE INDEX IF NOT EXISTS notification_deliveries_pending_idx
    ON notification_deliveries(next_attempt_at,id) WHERE status='pending';

CREATE TABLE IF NOT EXISTS notification_delivery_attempts (
    id bigserial PRIMARY KEY,
    delivery_id uuid NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
    attempt integer NOT NULL CHECK (attempt > 0),
    succeeded boolean NOT NULL,
    response_code integer NOT NULL DEFAULT 0,
    error text NOT NULL DEFAULT '',
    attempted_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(delivery_id,attempt)
);
CREATE INDEX IF NOT EXISTS notification_delivery_attempts_delivery_idx
    ON notification_delivery_attempts(delivery_id,attempt DESC);

CREATE OR REPLACE FUNCTION l4d2panel_enqueue_notification_deliveries()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO notification_deliveries(id,notification_id,channel_id)
    SELECT gen_random_uuid(), NEW.id, channel.id
    FROM notification_channels channel
    WHERE channel.enabled AND channel.deleted_at IS NULL
    ON CONFLICT(notification_id,channel_id) DO NOTHING;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS notifications_enqueue_deliveries ON notifications;
CREATE TRIGGER notifications_enqueue_deliveries
AFTER INSERT ON notifications
FOR EACH ROW EXECUTE FUNCTION l4d2panel_enqueue_notification_deliveries();

-- A deployment upgrading from an older release should deliver unread recent
-- notifications as well, without duplicating an existing outbox row.
INSERT INTO notification_deliveries(id,notification_id,channel_id)
SELECT gen_random_uuid(), notification.id, channel.id
FROM notifications notification
CROSS JOIN notification_channels channel
WHERE notification.created_at >= now()-interval '24 hours'
  AND channel.enabled AND channel.deleted_at IS NULL
ON CONFLICT(notification_id,channel_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS instance_update_state (
    instance_id uuid PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
    installed_build text NOT NULL DEFAULT '',
    available_build text NOT NULL DEFAULT '',
    available boolean NOT NULL DEFAULT false,
    checked_at timestamptz NOT NULL DEFAULT now()
);

-- Capability-gated update checks are read-only and must never take the
-- per-instance write-operation lock.
DROP INDEX IF EXISTS jobs_instance_active_idx;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id)
    WHERE instance_id IS NOT NULL
      AND status IN ('queued','dispatched','running')
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update');

WITH duplicates AS (
    SELECT id, row_number() OVER (PARTITION BY instance_id,type ORDER BY created_at,id) AS position
    FROM jobs
    WHERE instance_id IS NOT NULL
      AND status IN ('queued','dispatched','running')
      AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update')
)
UPDATE jobs SET status='canceled',message='superseded during monitoring migration',finished_at=now()
WHERE id IN (SELECT id FROM duplicates WHERE position>1);

CREATE UNIQUE INDEX IF NOT EXISTS jobs_read_probe_active_idx ON jobs(instance_id,type)
    WHERE instance_id IS NOT NULL
      AND status IN ('queued','dispatched','running')
      AND type IN ('a2s.info','a2s.players','rcon.players','list','check_update');
