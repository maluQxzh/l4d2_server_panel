package control

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddress     string
	DatabaseURL       string
	MigrationsDir     string
	BootstrapUsername string
	BootstrapPassword string
	BootstrapName     string
	MasterKey         []byte
	CookieSecure      bool
	SessionTTL        time.Duration
	PublicURL         string
	TrustedProxyNets  []*net.IPNet
	Timezone          string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		ListenAddress:     envAnyDefault([]string{"LISTEN_ADDR", "CONTROL_LISTEN"}, ":8080"),
		DatabaseURL:       envAnyDefault([]string{"DATABASE_URL", "CONTROL_DATABASE_URL"}, ""),
		MigrationsDir:     envAnyDefault([]string{"MIGRATIONS_DIR", "CONTROL_MIGRATIONS_DIR"}, "migrations"),
		BootstrapUsername: envAnyDefault([]string{"BOOTSTRAP_USERNAME", "CONTROL_BOOTSTRAP_USERNAME"}, "admin"),
		BootstrapPassword: envAnyDefault([]string{"BOOTSTRAP_PASSWORD", "CONTROL_BOOTSTRAP_PASSWORD"}, ""),
		BootstrapName:     envAnyDefault([]string{"BOOTSTRAP_DISPLAY_NAME", "CONTROL_BOOTSTRAP_DISPLAY_NAME"}, "System Administrator"),
		CookieSecure:      envAnyBool([]string{"COOKIE_SECURE", "CONTROL_COOKIE_SECURE"}, true),
		SessionTTL:        12 * time.Hour,
		PublicURL:         strings.TrimRight(envAnyDefault([]string{"PUBLIC_URL", "CONTROL_PUBLIC_URL"}, ""), "/"),
		Timezone:          envAnyDefault([]string{"TZ", "CONTROL_TIMEZONE"}, "Asia/Hong_Kong"),
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return Config{}, fmt.Errorf("invalid TZ %q", cfg.Timezone)
	}
	trustedRaw := envAnyDefault([]string{"TRUSTED_PROXIES", "CONTROL_TRUSTED_PROXIES"}, "127.0.0.1/32,::1/128")
	for _, raw := range strings.Split(trustedRaw, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid TRUSTED_PROXIES entry %q", raw)
		}
		cfg.TrustedProxyNets = append(cfg.TrustedProxyNets, network)
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if raw := envAnyDefault([]string{"SESSION_TTL", "CONTROL_SESSION_TTL"}, ""); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 15*time.Minute {
			return Config{}, fmt.Errorf("invalid SESSION_TTL")
		}
		cfg.SessionTTL = d
	}
	key, err := decodeKey(envAnyDefault([]string{"MASTER_KEY", "CONTROL_MASTER_KEY"}, ""))
	if err != nil {
		return Config{}, err
	}
	cfg.MasterKey = key
	return cfg, nil
}

func envAnyDefault(names []string, fallback string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envAnyBool(names []string, fallback bool) bool {
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return envBool(name, fallback)
		}
	}
	return fallback
}

func decodeKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("MASTER_KEY is required (32 bytes, base64 or hex)")
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	return nil, errors.New("MASTER_KEY must decode to exactly 32 bytes")
}
