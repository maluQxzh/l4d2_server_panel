package control

import (
	"strings"
	"testing"
)

func TestConfigAcceptsDeploymentEnvironmentNames(t *testing.T) {
	for _, name := range []string{"CONTROL_DATABASE_URL", "CONTROL_MASTER_KEY", "CONTROL_LISTEN", "CONTROL_MIGRATIONS_DIR", "CONTROL_COOKIE_SECURE"} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", "postgres://test")
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
