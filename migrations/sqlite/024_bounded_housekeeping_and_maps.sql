-- Bound housekeeping work without repeatedly scanning retained job history.
CREATE INDEX jobs_cleanup_background_idx ON jobs(status,finished_at,id)
WHERE status IN ('succeeded','failed','canceled') AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1 AND COALESCE(json_extract(result,'$.cancel_pending'),0)<>1 AND (type='rcon.players' OR (requested_by IS NULL AND type IN ('a2s.info','a2s.players','list','check_update')));
CREATE INDEX jobs_cleanup_transfer_idx ON jobs(finished_at,id)
WHERE status IN ('succeeded','failed','canceled') AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1 AND COALESCE(json_extract(result,'$.cancel_pending'),0)<>1 AND type IN ('logs.search','logs.chunk','backup.chunk','backup.upload.chunk');
CREATE INDEX jobs_cleanup_secrets_idx ON jobs(finished_at,id)
WHERE status IN ('succeeded','failed','canceled') AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1 AND COALESCE(json_extract(result,'$.cancel_pending'),0)<>1 AND (json_type(payload,'$.command_ciphertext') IS NOT NULL OR json_type(payload,'$.source_ciphertext') IS NOT NULL OR json_type(payload,'$.content_ciphertext') IS NOT NULL OR json_type(payload,'$.content_base64_ciphertext') IS NOT NULL OR json_type(payload,'$.share_url_ciphertext') IS NOT NULL OR json_type(payload,'$.archive_ciphertext') IS NOT NULL OR json_type(payload,'$.keyword_ciphertext') IS NOT NULL OR json_type(payload,'$.existing_rcon_password_ciphertext') IS NOT NULL OR json_type(result,'$.response_ciphertext') IS NOT NULL OR json_type(result,'$.content_base64_ciphertext') IS NOT NULL OR json_type(result,'$.entries_ciphertext') IS NOT NULL);
CREATE INDEX jobs_backup_upload_lookup_idx ON jobs(json_extract(payload,'$.upload_id'),status,json_extract(payload,'$.offset'))
WHERE type='backup.upload.chunk';
CREATE INDEX maps_created_id_idx ON maps(created_at DESC,id DESC);
