CREATE INDEX audit_events_resource_time_idx ON audit_events(resource_id,created_at DESC,id DESC);
CREATE INDEX audit_events_time_id_idx ON audit_events(created_at DESC,id DESC);
CREATE INDEX notifications_time_id_idx ON notifications(created_at DESC,id DESC);
