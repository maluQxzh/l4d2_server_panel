package control

import (
	"context"
	"database/sql"
)

const cleanupTerminalPredicate = `status IN ('succeeded','failed','canceled') AND COALESCE(json_extract(result,'$._projection_pending'),0)<>1 AND COALESCE(json_extract(result,'$.cancel_pending'),0)<>1`
const cleanupSecretsPredicate = `(json_type(payload,'$.command_ciphertext') IS NOT NULL OR json_type(payload,'$.source_ciphertext') IS NOT NULL OR json_type(payload,'$.content_ciphertext') IS NOT NULL OR json_type(payload,'$.content_base64_ciphertext') IS NOT NULL OR json_type(payload,'$.share_url_ciphertext') IS NOT NULL OR json_type(payload,'$.archive_ciphertext') IS NOT NULL OR json_type(payload,'$.keyword_ciphertext') IS NOT NULL OR json_type(payload,'$.existing_rcon_password_ciphertext') IS NOT NULL OR json_type(result,'$.response_ciphertext') IS NOT NULL OR json_type(result,'$.content_base64_ciphertext') IS NOT NULL OR json_type(result,'$.entries_ciphertext') IS NOT NULL)`
const cleanupTransferPredicate = `type IN ('logs.search','logs.chunk','backup.chunk','backup.upload.chunk')`
const housekeepingJobBatch = 500

func deleteOldJobs(ctx context.Context, tx *sql.Tx, index, predicate string, before any) error {
	// index and predicate come only from closed internal constants.
	_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id IN (
 SELECT id FROM jobs INDEXED BY `+index+` WHERE `+cleanupTerminalPredicate+` AND `+predicate+`
 AND finished_at<?1 ORDER BY finished_at,id LIMIT ?2)`, before, housekeepingJobBatch)
	return err
}

func clearOldJobSecrets(ctx context.Context, tx *sql.Tx, before any) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET
 payload=json_remove(payload,'$.command_ciphertext','$.source_ciphertext','$.content_ciphertext','$.content_base64_ciphertext','$.share_url_ciphertext','$.archive_ciphertext','$.keyword_ciphertext','$.existing_rcon_password_ciphertext'),
 result=json_remove(result,'$.response_ciphertext','$.content_base64_ciphertext','$.entries_ciphertext')
 WHERE id IN (SELECT id FROM jobs INDEXED BY jobs_cleanup_secrets_idx WHERE `+cleanupTerminalPredicate+` AND `+cleanupSecretsPredicate+`
 AND finished_at<?1 ORDER BY finished_at,id LIMIT ?2)`, before, housekeepingJobBatch)
	return err
}
