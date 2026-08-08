-- Serialize game installation deletion with every other installation write.
DROP INDEX jobs_active_installation_write_idx;
CREATE UNIQUE INDEX jobs_active_installation_write_idx ON jobs(game_installation_id)
WHERE game_installation_id IS NOT NULL AND status IN ('queued','dispatched','running')
  AND type IN ('game.install','game.import','game.validate','game.update','game.reinstall','game.delete');
