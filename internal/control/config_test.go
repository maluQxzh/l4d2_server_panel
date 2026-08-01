package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigAcceptsDeploymentEnvironmentNames(t *testing.T) {
	for _, name := range []string{"CONTROL_DATABASE_PATH", "CONTROL_MASTER_KEY", "CONTROL_LISTEN", "CONTROL_MIGRATIONS_DIR", "CONTROL_COOKIE_SECURE"} {
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
  "listen_address": "127.0.0.1:8080",
  "database_path": "data\\control.sqlite3",
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
		"missing":  `{"listen_address":"127.0.0.1:8080"}`,
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
