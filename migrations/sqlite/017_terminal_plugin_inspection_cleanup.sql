UPDATE jobs
SET payload=json_remove(payload,'$.content_base64_ciphertext','$.source_ciphertext'),
    result=json_remove(result,'$._projection_pending')
WHERE type='inspect_plugin_archive'
  AND status IN ('succeeded','failed','canceled')
  AND (
    json_type(payload,'$.content_base64_ciphertext') IS NOT NULL
    OR json_type(payload,'$.source_ciphertext') IS NOT NULL
    OR json_extract(result,'$._projection_pending')=1
  );
