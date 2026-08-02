CREATE TABLE bootstrap_setup_key (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    key_hash BLOB NOT NULL CHECK (length(key_hash) = 32),
    created_at TIMESTAMP NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000000 AS INTEGER))
);
