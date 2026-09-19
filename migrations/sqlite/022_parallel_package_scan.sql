-- Scanning reads the instance; stale projections are rejected using a Control-owned generation.
DROP INDEX jobs_instance_active_idx;
ALTER TABLE jobs DROP COLUMN holds_instance_write_lock;
ALTER TABLE jobs DROP COLUMN is_instance_write;
ALTER TABLE jobs ADD COLUMN is_instance_write INTEGER GENERATED ALWAYS AS (
    CASE WHEN instance_id IS NOT NULL
      AND type NOT IN ('a2s.info','a2s.players','rcon.players','list','check_update','logs.search','logs.chunk','backup.chunk','backup.upload.abort','inspect_plugin_archive','prepare_package','discard_prepared_package','prepared_install_queue','scan_packages')
      AND NOT (type='file' AND json_extract(payload,'$.operation')='read')
    THEN 1 ELSE 0 END
) VIRTUAL;
ALTER TABLE jobs ADD COLUMN holds_instance_write_lock INTEGER GENERATED ALWAYS AS (
    CASE WHEN is_instance_write=1
      AND (status IN ('queued','dispatched','running')
        OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1)
        OR (status IN ('succeeded','failed') AND json_extract(result,'$._projection_pending')=1))
    THEN 1 ELSE 0 END
) VIRTUAL;
CREATE UNIQUE INDEX jobs_instance_active_idx ON jobs(instance_id) WHERE holds_instance_write_lock=1;

ALTER TABLE jobs ADD COLUMN holds_instance_scan_slot INTEGER GENERATED ALWAYS AS (
    CASE WHEN instance_id IS NOT NULL AND type='scan_packages'
      AND (status IN ('queued','dispatched','running')
        OR (status='canceled' AND json_extract(result,'$.cancel_pending')=1)
        OR (status IN ('succeeded','failed') AND json_extract(result,'$._projection_pending')=1))
    THEN 1 ELSE 0 END
) VIRTUAL;
CREATE UNIQUE INDEX jobs_instance_scan_active_idx ON jobs(instance_id) WHERE holds_instance_scan_slot=1;

ALTER TABLE instances ADD COLUMN package_scan_generation INTEGER NOT NULL DEFAULT 0
    CHECK(typeof(package_scan_generation)='integer' AND package_scan_generation>=0);
CREATE TABLE package_scan_runs (
    job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    eligible INTEGER NOT NULL CHECK(eligible IN (0,1)),
    outcome TEXT NOT NULL DEFAULT 'pending' CHECK(outcome IN ('pending','applied','stale')),
    registered_count INTEGER NOT NULL DEFAULT 0 CHECK(registered_count BETWEEN 0 AND 1000)
);
-- Old requests have no trustworthy snapshot. Preserve jobs/results, but never
-- replay their inventory into a newer package list (including after upgrade).
INSERT INTO package_scan_runs(job_id,instance_id,generation,eligible,outcome)
    SELECT id,instance_id,0,0,
      CASE WHEN status='succeeded' AND COALESCE(json_extract(result,'$._projection_pending'),0)=0 THEN 'stale' ELSE 'pending' END
    FROM jobs WHERE type='scan_packages' AND instance_id IS NOT NULL;

CREATE TRIGGER package_scan_snapshot AFTER INSERT ON jobs
WHEN NEW.type='scan_packages' AND NEW.instance_id IS NOT NULL
BEGIN
    INSERT INTO package_scan_runs(job_id,instance_id,generation,eligible)
        SELECT NEW.id,id,package_scan_generation,
          NOT EXISTS(SELECT 1 FROM jobs WHERE instance_id=NEW.instance_id AND holds_instance_write_lock=1)
        FROM instances WHERE id=NEW.instance_id AND node_id=NEW.node_id;
END;
CREATE TRIGGER package_scan_write_created AFTER INSERT ON jobs WHEN NEW.is_instance_write=1
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id=NEW.instance_id;
END;
CREATE TRIGGER package_scan_write_barrier_changed AFTER UPDATE OF status,result,payload,instance_id,type ON jobs
WHEN (OLD.is_instance_write=1 OR NEW.is_instance_write=1)
  AND (OLD.holds_instance_write_lock<>NEW.holds_instance_write_lock OR OLD.instance_id IS NOT NEW.instance_id)
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (OLD.instance_id,NEW.instance_id);
END;
CREATE TRIGGER package_scan_write_deleted AFTER DELETE ON jobs WHEN OLD.holds_instance_write_lock=1
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id=OLD.instance_id;
END;
CREATE TRIGGER package_scan_target_changed AFTER UPDATE OF node_id,path,installation_id ON instances
WHEN OLD.node_id IS NOT NEW.node_id OR OLD.path IS NOT NEW.path OR OLD.installation_id IS NOT NEW.installation_id
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id=NEW.id;
END;
-- Include direct/replayed inventory projections, not just task creation.
CREATE TRIGGER package_scan_addon_packages_insert AFTER INSERT ON addon_packages
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (NEW.instance_id);
END;
CREATE TRIGGER package_scan_addon_packages_update AFTER UPDATE ON addon_packages
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (OLD.instance_id,NEW.instance_id);
END;
CREATE TRIGGER package_scan_addon_packages_delete AFTER DELETE ON addon_packages
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (OLD.instance_id);
END;
CREATE TRIGGER package_scan_addon_package_files_insert AFTER INSERT ON addon_package_files
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (NEW.instance_id);
END;
CREATE TRIGGER package_scan_addon_package_files_update AFTER UPDATE ON addon_package_files
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (OLD.instance_id,NEW.instance_id);
END;
CREATE TRIGGER package_scan_addon_package_files_delete AFTER DELETE ON addon_package_files
BEGIN
    UPDATE instances SET package_scan_generation=package_scan_generation+1 WHERE id IN (OLD.instance_id);
END;
