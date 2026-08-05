package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Applied SQLite migration bodies are immutable: Store.Migrate persists these
// checksums and deliberately refuses to start if an old file is rewritten.
func TestSQLiteMigrationChecksums(t *testing.T) {
	t.Parallel()
	expected := map[string]string{
		"001_control_plane.sql":               "5a95514ce69e52e64613fbd1313eeed81eae422dc917c3e6f7acaae53e2f508f",
		"002_bootstrap_setup_key.sql":         "403054323dc5d62600b26aebdcdcadbabea46cbde0f6b83f136c37c2e4ff3fa1",
		"003_system_update_jobs.sql":          "508c18088158cc8f1be897e30f510e207d8e2999c1762569216a9bf2d36ddea3",
		"004_runtime_environments.sql":        "6f8ff59e71d5d8cabeaf5e1dae1016fcea287c3e99dbcc8e63d2c4fcfa0c1a61",
		"005_absolute_game_installations.sql": "070af912fa68bf2549819d0a0dac7e8dbe916fd98453cfbd803e8feaac073e96",
		"006_remove_config_history.sql":       "468fd664f7f60907388db13edb051ecde268dae82ce6e07fb5e3893ae03c43a5",
		"007_map_library.sql":                 "8ff1eb6583f458254ef8add0e7928e26523668ae15423e5d1599e77e401fbd5e",
	}
	root := filepath.Join("..", "..", "migrations", "sqlite")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	actual := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		actual++
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if got, want := hex.EncodeToString(sum[:]), expected[entry.Name()]; want == "" || got != want {
			t.Errorf("SQLite migration %s checksum=%s want=%s", entry.Name(), got, want)
		}
	}
	if actual != len(expected) {
		t.Fatalf("SQLite migration count=%d checksum count=%d", actual, len(expected))
	}
}

func TestRemoveConfigHistoryMigrationPreservesRestoreProjectionMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sourceRoot := filepath.Join("..", "..", "migrations", "sqlite")
	migrationRoot := t.TempDir()
	copyMigration := func(name string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(migrationRoot, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"001_control_plane.sql",
		"002_bootstrap_setup_key.sql",
		"003_system_update_jobs.sql",
		"004_runtime_environments.sql",
		"005_absolute_game_installations.sql",
	} {
		copyMigration(name)
	}
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "pre-history-removal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, migrationRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO nodes(id,name,status) VALUES('node-1','node-1','online');
		INSERT INTO instances(id,node_id,name,status,port,config) VALUES('instance-1','node-1','instance-1','stopped',27015,'{}');
		INSERT INTO jobs(id,node_id,instance_id,type,status) VALUES('restore-job-1','node-1','instance-1','restore','succeeded');
		INSERT INTO config_versions(id,instance_id,version,config,source_job_id) VALUES('config-version-1','instance-1',2,'{}','restore-job-1');
	`); err != nil {
		t.Fatal(err)
	}
	copyMigration("006_remove_config_history.sql")
	if err := store.Migrate(ctx, migrationRoot); err != nil {
		t.Fatal(err)
	}
	var instanceID string
	if err := store.db.QueryRowContext(ctx, `SELECT instance_id FROM restore_config_projections WHERE job_id='restore-job-1'`).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	if instanceID != "instance-1" {
		t.Fatalf("restore projection instance=%q", instanceID)
	}
	var oldTableCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='config_versions'`).Scan(&oldTableCount); err != nil {
		t.Fatal(err)
	}
	if oldTableCount != 0 {
		t.Fatalf("config_versions table still exists after migration")
	}
}
