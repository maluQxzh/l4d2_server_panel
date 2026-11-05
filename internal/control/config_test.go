package control

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestConfigDefaultListenerIs19090(t *testing.T) {
	for _, name := range []string{"LISTEN_ADDR", "CONTROL_LISTEN", "MASTER_KEY", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("MASTER_KEY", strings.Repeat("01", 32))
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != ":19090" {
		t.Fatalf("default listen address = %q, want :19090", cfg.ListenAddress)
	}
}

func TestConfigAcceptsDeploymentEnvironmentNames(t *testing.T) {
	for _, name := range []string{"CONTROL_DATABASE_PATH", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE", "CONTROL_LISTEN", "CONTROL_MIGRATIONS_DIR", "CONTROL_COOKIE_SECURE"} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "control.sqlite3"))
	t.Setenv("MASTER_KEY", strings.Repeat("01", 32))
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("MIGRATIONS_DIR", "/migrations")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("TRUSTED_PROXIES", "172.16.0.0/12")
	t.Setenv("TZ", "Asia/Hong_Kong")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != ":9000" || cfg.MigrationsDir != "/migrations" || cfg.CookieSecure || len(cfg.TrustedProxyNets) != 1 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestConfigFromFileIsStrictAndEnvironmentWins(t *testing.T) {
	for _, name := range []string{
		"DATABASE_PATH", "CONTROL_DATABASE_PATH", "MASTER_KEY", "CONTROL_MASTER_KEY",
		"MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE",
		"LISTEN_ADDR", "CONTROL_LISTEN", "MIGRATIONS_DIR", "CONTROL_MIGRATIONS_DIR",
		"COOKIE_SECURE", "CONTROL_COOKIE_SECURE", "PUBLIC_URL", "CONTROL_PUBLIC_URL",
		"TRUSTED_PROXIES", "CONTROL_TRUSTED_PROXIES", "TZ", "CONTROL_TIMEZONE",
		"LOG_FILE", "CONTROL_LOG_FILE",
	} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "control.json")
	data := `{
  "schema_version": 2,
  "listen_address": "127.0.0.1:19090",
  "database_path": "data/control.sqlite3",
  "migrations_dir": "C:\\Program Files\\L4D2Panel\\Control\\migrations",
  "master_key": "` + strings.Repeat("ab", 32) + `",
  "cookie_secure": true,
  "session_ttl": "24h",
  "public_url": "https://panel.example.test:9443/",
  "trusted_proxies": ["127.0.0.1/32"],
  "timezone": "Asia/Hong_Kong",
  "log_file": "C:\\ProgramData\\L4D2Panel\\logs\\control.jsonl"
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_LISTEN", "127.0.0.1:8181")
	t.Setenv("CONTROL_COOKIE_SECURE", "false")

	cfg, err := ConfigFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "127.0.0.1:8181" || cfg.CookieSecure {
		t.Fatalf("environment did not override file config: %+v", cfg)
	}
	if cfg.SessionTTL != 24*time.Hour || cfg.PublicURL != "https://panel.example.test:9443" {
		t.Fatalf("unexpected parsed config: %+v", cfg)
	}
	if len(cfg.TrustedProxyNets) != 1 || cfg.LogFile == "" {
		t.Fatalf("missing file-only settings: %+v", cfg)
	}
	if cfg.DatabasePath != filepath.Join(filepath.Dir(path), "data", "control.sqlite3") {
		t.Fatalf("database path was not resolved relative to config: %q", cfg.DatabasePath)
	}
}

func TestConfigFromFileRejectsUnknownAndTrailingData(t *testing.T) {
	t.Setenv("CONTROL_DATABASE_PATH", "")
	t.Setenv("DATABASE_PATH", "")
	for name, body := range map[string]string{
		"unknown":  `{"schema_version":2,"unknown":true}`,
		"missing":  `{"listen_address":"127.0.0.1:19090"}`,
		"trailing": `{"schema_version":2} {"schema_version":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "control.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ConfigFromFile(path); err == nil {
				t.Fatal("expected strict config error")
			}
		})
	}
}

func TestConfigGeneratesAndReusesPersistentMasterKeyFile(t *testing.T) {
	for _, name := range []string{"MASTER_KEY", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "secrets", "control-master.key")
	t.Setenv("MASTER_KEY_FILE", path)

	first, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.MasterKey) != 32 {
		t.Fatalf("generated master key length = %d, want 32", len(first.MasterKey))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("master key file mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, first.MasterKey) {
		t.Fatal("generated key does not match persistent key file")
	}

	second, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.MasterKey, second.MasterKey) {
		t.Fatal("persistent master key changed between loads")
	}
}

func TestConfigSeedsPersistentMasterKeyAndRejectsMismatch(t *testing.T) {
	for _, name := range []string{"MASTER_KEY", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "control-master.key")
	t.Setenv("MASTER_KEY_FILE", path)
	t.Setenv("MASTER_KEY", strings.Repeat("01", 32))

	seeded, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seeded.MasterKey, bytes.Repeat([]byte{1}, 32)) {
		t.Fatal("explicit MASTER_KEY did not seed the persistent key file")
	}

	t.Setenv("MASTER_KEY", strings.Repeat("02", 32))
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched persistent master key was accepted: %v", err)
	}
}

func TestConfigRejectsSymlinkMasterKeyFile(t *testing.T) {
	for _, name := range []string{"MASTER_KEY", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "control-master.key")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("MASTER_KEY_FILE", link)
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("symlink MASTER_KEY_FILE was accepted: %v", err)
	}
}

func TestConfigRejectsMasterKeyFileAtDatabasePath(t *testing.T) {
	for _, name := range []string{"MASTER_KEY", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "control.sqlite3")
	t.Setenv("DATABASE_PATH", path)
	t.Setenv("MASTER_KEY_FILE", path)
	if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("database path was accepted as MASTER_KEY_FILE: %v", err)
	}
}

func TestConfigDoesNotChangeExistingMasterKeyDirectoryMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix directory modes")
	}
	for _, name := range []string{"MASTER_KEY", "CONTROL_MASTER_KEY", "MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"} {
		t.Setenv(name, "")
	}
	dir := filepath.Join(t.TempDir(), "shared-data")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASTER_KEY_FILE", filepath.Join(dir, "control-master.key"))
	if _, err := ConfigFromEnv(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("existing master key directory mode = %o, want 750", got)
	}
}
