package control

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Applied migration bodies are immutable: Store.Migrate persists these
// checksums and deliberately refuses to start if an old file is rewritten.
// Schema changes must therefore be added as a new, monotonically numbered
// migration and its checksum recorded here once.
func TestReleasedMigrationChecksums(t *testing.T) {
	t.Parallel()
	expected := map[string]string{
		"001_control_plane.sql":              "4c76cc505cc9039e881c683e65be35cdea63ad236d7eb0908718f8ddbe9860e3",
		"002_security.sql":                   "8205cb932265e9a3f9f43d7381eaba2c14fc02aa80497a94c41d9bb4a3cd6c6f",
		"003_monitoring.sql":                 "8d8ca2c24d49a2f5bdad2ca93aaa7a1eb81bffc969e524d0f345094c929cf910",
		"004_role_bindings.sql":              "497ccb9c5c9c46fc27325bf422be7717199cdc0550566116f020e1b30195defa",
		"005_transfer_jobs.sql":              "1b85608aec9f86b4e1b83d7097b9f8d955e8cba6019c7a1a9a08676004bb6e48",
		"006_player_bans.sql":                "2c679b26f406f408f2a3f34694e1e73606d25f8e70cbe96269da8c9a2098aa27",
		"007_read_job_lock.sql":              "d526a0be3726dee5942dbd5926c336a91b261f867c564efb717e84ef2dfc97c6",
		"008_package_versions.sql":           "95cb1db271b996093bb04057974d4984ffa8a5f377ed2ff06631d5fcc2a9802a",
		"009_instance_port_reservations.sql": "d15b1b15e1fa77dc3321e30a19424b1f0f65b831fd0b8af1f3c86a3c0783dcc1",
		"010_quarantine_entries.sql":         "f62ae2ac21a061bd836a8d61550a84cf0db9058a5dcd09999f2bf6de1964f1d4",
		"011_backup_uploads.sql":             "4f60d3e23743535c1ed3c3c15b7124c3492ddf312fd51095fd16f79b8e0348ef",
		"012_job_request_fingerprints.sql":   "174f028e4eb3249846b931f2a847a42db8d78ee3c3d46a7f403fa47d79597d04",
		"013_restore_config_projection.sql":  "2c32b6d8334d6401c86560f96a206a8fc8e4720458cd88fd8192bbd6907ecb46",
		"014_cancel_pending_write_lock.sql":  "6e4d915585865daf1e224b74333ecba94df249b5e77536bff1e1fd5b06a6e9ff",
		"015_backup_upload_abort_lock.sql":   "40e3c7538a7f340570a36a74655aa57a9bda1a251a625265c121c48a7e7e84f8",
		"016_terminal_projection_lock.sql":   "7a0110c38d5a2ca4f3e6eb4f5c4868259909605b526ba04e4ddf28224d30a58f",
	}

	root := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	actualCount := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		actualCount++
		want, ok := expected[entry.Name()]
		if !ok {
			t.Errorf("migration %s has no immutable checksum; add it to this test", entry.Name())
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Errorf("read migration %s: %v", entry.Name(), err)
			continue
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("migration %s was rewritten: got %s want %s; add a new migration instead", entry.Name(), got, want)
		}
	}
	if actualCount != len(expected) {
		t.Errorf("migration count=%d, checksum count=%d", actualCount, len(expected))
	}
}

func TestSQLiteMigrationChecksums(t *testing.T) {
	t.Parallel()
	expected := map[string]string{
		"001_control_plane.sql":       "5a95514ce69e52e64613fbd1313eeed81eae422dc917c3e6f7acaae53e2f508f",
		"002_bootstrap_setup_key.sql": "403054323dc5d62600b26aebdcdcadbabea46cbde0f6b83f136c37c2e4ff3fa1",
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
