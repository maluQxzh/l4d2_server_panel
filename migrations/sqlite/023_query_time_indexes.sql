-- Range scans for rollups and retention must not traverse every resource.
CREATE INDEX metrics_time_idx ON metrics(collected_at);
CREATE INDEX metrics_1m_time_idx ON metrics_1m(bucket);
CREATE INDEX metrics_1h_time_idx ON metrics_1h(bucket);
-- Stable history pagination and active-task filtering.
CREATE INDEX jobs_created_id_idx ON jobs(created_at DESC,id DESC);
CREATE INDEX jobs_active_created_id_idx ON jobs(created_at DESC,id DESC)
WHERE status IN ('queued','dispatched','running');
