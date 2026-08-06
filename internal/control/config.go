package control

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	ConfigPath        string
	PortableService   bool
	ListenAddress     string
	DatabasePath      string
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
	LogFile           string
}

func ConfigFromEnv() (Config, error) {
	return buildConfig(configFile{}, "")
}

type configFile struct {
	SchemaVersion  int       `json:"schema_version"`
	ListenAddress  string    `json:"listen_address"`
	DatabasePath   string    `json:"database_path"`
	MigrationsDir  string    `json:"migrations_dir"`
	MasterKey      string    `json:"master_key"`
	CookieSecure   *bool     `json:"cookie_secure"`
	SessionTTL     string    `json:"session_ttl"`
	PublicURL      string    `json:"public_url"`
	TrustedProxies *[]string `json:"trusted_proxies"`
	Timezone       string    `json:"timezone"`
	LogFile        string    `json:"log_file"`
}

const maxControlConfigBytes = 64 << 10

// ConfigFromFile loads a strict, versioned JSON configuration and then applies
// environment overrides. This keeps the Docker deployment environment-based
// while allowing a Windows service to receive only a non-secret config path on
// its command line.
func ConfigFromFile(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Config{}, errors.New("control config path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open control config: %w", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxControlConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read control config: %w", err)
	}
	if len(body) > maxControlConfigBytes {
		return Config{}, fmt.Errorf("control config exceeds %d bytes", maxControlConfigBytes)
	}
	var source configFile
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return Config{}, fmt.Errorf("decode control config: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Config{}, fmt.Errorf("decode control config: %w", err)
	}
	if source.SchemaVersion != 2 {
		return Config{}, fmt.Errorf("unsupported control config schema_version %d", source.SchemaVersion)
	}
	cfg, err := buildConfig(source, filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve control config path: %w", err)
	}
	cfg.ConfigPath = absPath
	return cfg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values are not allowed")
	}
	return err
}

func buildConfig(source configFile, configDir string) (Config, error) {
	cfg := Config{
		ListenAddress:     valueDefault(source.ListenAddress, ":19090"),
		DatabasePath:      valueDefault(source.DatabasePath, filepath.Join("data", "l4d2panel.sqlite3")),
		MigrationsDir:     valueDefault(source.MigrationsDir, filepath.Join("migrations", "sqlite")),
		BootstrapUsername: envAnyDefault([]string{"BOOTSTRAP_USERNAME", "CONTROL_BOOTSTRAP_USERNAME"}, "admin"),
		BootstrapPassword: envAnyDefault([]string{"BOOTSTRAP_PASSWORD", "CONTROL_BOOTSTRAP_PASSWORD"}, ""),
		BootstrapName:     envAnyDefault([]string{"BOOTSTRAP_DISPLAY_NAME", "CONTROL_BOOTSTRAP_DISPLAY_NAME"}, "System Administrator"),
		CookieSecure:      true,
		SessionTTL:        12 * time.Hour,
		PublicURL:         strings.TrimRight(strings.TrimSpace(source.PublicURL), "/"),
		Timezone:          valueDefault(source.Timezone, "Asia/Hong_Kong"),
		LogFile:           strings.TrimSpace(source.LogFile),
	}
	if source.CookieSecure != nil {
		cfg.CookieSecure = *source.CookieSecure
	}
	if raw := strings.TrimSpace(source.SessionTTL); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 15*time.Minute {
			return Config{}, fmt.Errorf("invalid session_ttl")
		}
		cfg.SessionTTL = d
	}
	trustedRaw := "127.0.0.1/32,::1/128"
	if source.TrustedProxies != nil {
		trustedRaw = strings.Join(*source.TrustedProxies, ",")
	}
	masterKeyRaw := strings.TrimSpace(source.MasterKey)
	masterKeyFile := ""

	cfg.ListenAddress = envAnyDefault([]string{"LISTEN_ADDR", "CONTROL_LISTEN"}, cfg.ListenAddress)
	if databasePath := envAnyDefault([]string{"DATABASE_PATH", "CONTROL_DATABASE_PATH"}, ""); databasePath != "" {
		cfg.DatabasePath = databasePath
		configDir = ""
	}
	cfg.MigrationsDir = envAnyDefault([]string{"MIGRATIONS_DIR", "CONTROL_MIGRATIONS_DIR"}, cfg.MigrationsDir)
	cfg.CookieSecure = envAnyBool([]string{"COOKIE_SECURE", "CONTROL_COOKIE_SECURE"}, cfg.CookieSecure)
	cfg.PublicURL = strings.TrimRight(envAnyDefault([]string{"PUBLIC_URL", "CONTROL_PUBLIC_URL"}, cfg.PublicURL), "/")
	cfg.Timezone = envAnyDefault([]string{"TZ", "CONTROL_TIMEZONE"}, cfg.Timezone)
	cfg.LogFile = envAnyDefault([]string{"LOG_FILE", "CONTROL_LOG_FILE"}, cfg.LogFile)
	trustedRaw = envAnyDefault([]string{"TRUSTED_PROXIES", "CONTROL_TRUSTED_PROXIES"}, trustedRaw)
	masterKeyRaw = envAnyDefault([]string{"MASTER_KEY", "CONTROL_MASTER_KEY"}, masterKeyRaw)
	masterKeyFile = envAnyDefault([]string{"MASTER_KEY_FILE", "CONTROL_MASTER_KEY_FILE"}, masterKeyFile)
	if raw := envAnyDefault([]string{"SESSION_TTL", "CONTROL_SESSION_TTL"}, ""); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 15*time.Minute {
			return Config{}, fmt.Errorf("invalid SESSION_TTL")
		}
		cfg.SessionTTL = d
	}

	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return Config{}, fmt.Errorf("invalid TZ %q", cfg.Timezone)
	}
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
	if !filepath.IsAbs(cfg.DatabasePath) {
		base := configDir
		if base == "" {
			var err error
			base, err = os.Getwd()
			if err != nil {
				return Config{}, fmt.Errorf("resolve database working directory: %w", err)
			}
		}
		cfg.DatabasePath = filepath.Join(base, cfg.DatabasePath)
	}
	absDatabasePath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve database path: %w", err)
	}
	cfg.DatabasePath = absDatabasePath
	if strings.TrimSpace(masterKeyFile) != "" && filepath.Clean(masterKeyFile) == cfg.DatabasePath {
		return Config{}, errors.New("MASTER_KEY_FILE must differ from DATABASE_PATH")
	}
	key, err := resolveMasterKey(masterKeyRaw, masterKeyFile)
	if err != nil {
		return Config{}, err
	}
	cfg.MasterKey = key
	return cfg, nil
}

const maxMasterKeyFileBytes = 256

func resolveMasterKey(raw, path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return decodeKey(raw)
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("MASTER_KEY_FILE must be an absolute path")
	}

	var provided []byte
	if strings.TrimSpace(raw) != "" {
		var err error
		provided, err = decodeKey(raw)
		if err != nil {
			return nil, err
		}
	}

	stored, err := readMasterKeyFile(path)
	if err == nil {
		if provided != nil && !bytes.Equal(provided, stored) {
			return nil, errors.New("MASTER_KEY does not match the persistent MASTER_KEY_FILE")
		}
		return stored, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key := provided
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate persistent master key: %w", err)
		}
	}
	if err := createMasterKeyFile(path, key); err != nil {
		if errors.Is(err, os.ErrExist) {
			stored, readErr := readMasterKeyFile(path)
			if readErr != nil {
				return nil, readErr
			}
			if provided != nil && !bytes.Equal(provided, stored) {
				return nil, errors.New("MASTER_KEY does not match the persistent MASTER_KEY_FILE")
			}
			return stored, nil
		}
		return nil, err
	}
	return key, nil
}

func readMasterKeyFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("MASTER_KEY_FILE must be a regular file and not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open MASTER_KEY_FILE: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect MASTER_KEY_FILE: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return nil, errors.New("MASTER_KEY_FILE changed while it was being opened")
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure MASTER_KEY_FILE: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxMasterKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read MASTER_KEY_FILE: %w", err)
	}
	if len(body) > maxMasterKeyFileBytes {
		return nil, errors.New("MASTER_KEY_FILE is too large")
	}
	key, err := decodeKey(string(body))
	if err != nil {
		return nil, fmt.Errorf("invalid MASTER_KEY_FILE: %w", err)
	}
	return key, nil
}

func createMasterKeyFile(path string, key []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create MASTER_KEY_FILE directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if _, err := io.WriteString(file, encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write MASTER_KEY_FILE: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync MASTER_KEY_FILE: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close MASTER_KEY_FILE: %w", err)
	}
	return nil
}

func valueDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
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
