ALTER TABLE addon_packages ADD COLUMN file_count INTEGER NOT NULL DEFAULT 0 CHECK (file_count>=0);
ALTER TABLE addon_packages ADD COLUMN restart_required INTEGER NOT NULL DEFAULT 0 CHECK (restart_required IN (0,1));

CREATE TABLE addon_package_files (
    package_id TEXT NOT NULL REFERENCES addon_packages(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    sha256 TEXT NOT NULL CHECK (length(sha256)=64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    size INTEGER NOT NULL CHECK (size>=0),
    entrypoint INTEGER NOT NULL DEFAULT 0 CHECK (entrypoint IN (0,1)),
    PRIMARY KEY(package_id,path)
);

CREATE UNIQUE INDEX addon_package_files_instance_path_idx
ON addon_package_files(instance_id,lower(path));
