ALTER TABLE addon_packages ADD COLUMN IF NOT EXISTS last_job_id uuid;

CREATE TABLE IF NOT EXISTS addon_package_versions (
    id uuid PRIMARY KEY,
    package_id uuid NOT NULL REFERENCES addon_packages(id) ON DELETE CASCADE,
    instance_id uuid NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    source_job_id uuid NOT NULL,
    version text NOT NULL DEFAULT '',
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    storage_path text NOT NULL,
    target_path text NOT NULL,
    manifest jsonb NOT NULL DEFAULT '{"files":[]}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(source_job_id, storage_path)
);

CREATE INDEX IF NOT EXISTS addon_package_versions_package_idx
    ON addon_package_versions(package_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS addon_package_versions_instance_idx
    ON addon_package_versions(instance_id, created_at DESC, id);
