package control

import (
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
