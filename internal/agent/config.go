package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHeartbeatInterval = 10 * time.Second
	defaultMetricsInterval   = 15 * time.Second
	defaultReconnectMin      = time.Second
	defaultReconnectMax      = time.Minute
)

// Config is intentionally dependency-free so the same agent binary can read
// either JSON or the small, conventional YAML subset used by our installers.
type Config struct {
	CentralURL          string   `json:"central_url"`
	EnrollmentToken     string   `json:"enrollment_token,omitempty"`
	Credential          string   `json:"credential,omitempty"`
	NodeID              string   `json:"node_id,omitempty"`
	NodeName            string   `json:"node_name,omitempty"`
	InstanceRoots       []string `json:"instance_roots"`
	SteamCMDPath        string   `json:"steamcmd_path"`
	DataDir             string   `json:"data_dir"`
	HeartbeatInterval   string   `json:"heartbeat_interval,omitempty"`
	MetricsInterval     string   `json:"metrics_interval,omitempty"`
	ReconnectMinDelay   string   `json:"reconnect_min_delay,omitempty"`
	ReconnectMaxDelay   string   `json:"reconnect_max_delay,omitempty"`
	SteamCMDConcurrency int      `json:"steamcmd_concurrency,omitempty"`
	InsecureSkipVerify  bool     `json:"insecure_skip_verify,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := new(Config)
	if json.Valid(raw) {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("decode JSON config: %w", err)
		}
	} else {
		values, err := parseSimpleYAML(raw)
		if err != nil {
			return nil, fmt.Errorf("decode YAML config: %w", err)
		}
		converted, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(converted, cfg); err != nil {
			return nil, fmt.Errorf("decode YAML config values: %w", err)
		}
	}
	if err := cfg.normalize(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize(configPath string) error {
	if c.NodeName == "" {
		name, _ := os.Hostname()
		c.NodeName = name
	}
	base := filepath.Dir(configPath)
	if c.DataDir == "" {
		c.DataDir = filepath.Join(base, "data")
	}
	var err error
	c.DataDir, err = absoluteFrom(base, c.DataDir)
	if err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	if len(c.InstanceRoots) == 0 {
		c.InstanceRoots = []string{filepath.Join(c.DataDir, "instances")}
	}
	for i, root := range c.InstanceRoots {
		c.InstanceRoots[i], err = absoluteFrom(base, root)
		if err != nil {
			return fmt.Errorf("instance_roots[%d]: %w", i, err)
		}
	}
	if c.SteamCMDPath != "" {
		c.SteamCMDPath, err = absoluteFrom(base, c.SteamCMDPath)
		if err != nil {
			return fmt.Errorf("steamcmd_path: %w", err)
		}
	}
	if c.SteamCMDConcurrency <= 0 {
		c.SteamCMDConcurrency = 1
	}
	if c.SteamCMDConcurrency > 4 {
		return errors.New("steamcmd_concurrency must be between 1 and 4")
	}
	if _, err := c.heartbeatEvery(); err != nil {
		return err
	}
	if _, err := c.metricsEvery(); err != nil {
		return err
	}
	minDelay, err := c.reconnectMin()
	if err != nil {
		return err
	}
	maxDelay, err := c.reconnectMax()
	if err != nil {
		return err
	}
	if minDelay > maxDelay {
		return errors.New("reconnect_min_delay cannot exceed reconnect_max_delay")
	}
	return c.validateURL()
}

func absoluteFrom(base, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("path is empty")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	return filepath.Abs(filepath.Clean(value))
}

func (c *Config) validateURL() error {
	u, err := url.Parse(c.CentralURL)
	if err != nil || u.Host == "" {
		return errors.New("central_url must be an absolute URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return nil
	case "http", "ws":
		if plaintextControlHostAllowed(u.Hostname()) {
			return nil
		}
		return errors.New("central_url must use TLS (https or wss) outside localhost or a private/link-local IP address")
	default:
		return errors.New("central_url scheme must be https, wss, http, or ws")
	}
}

func plaintextControlHostAllowed(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// url.Hostname retains an IPv6 zone identifier. ParseIP deliberately does
	// not accept it, so remove only the zone suffix before classifying the
	// literal address. Hostnames are not resolved here: allowing a DNS name
	// based on one lookup would let a later DNS-rebinding response expose the
	// enrollment token or long-lived Agent credential over plaintext.
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func durationOr(value string, fallback time.Duration, field string) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", field)
	}
	return d, nil
}

func (c *Config) heartbeatEvery() (time.Duration, error) {
	return durationOr(c.HeartbeatInterval, defaultHeartbeatInterval, "heartbeat_interval")
}
func (c *Config) metricsEvery() (time.Duration, error) {
	return durationOr(c.MetricsInterval, defaultMetricsInterval, "metrics_interval")
}
func (c *Config) reconnectMin() (time.Duration, error) {
	return durationOr(c.ReconnectMinDelay, defaultReconnectMin, "reconnect_min_delay")
}
func (c *Config) reconnectMax() (time.Duration, error) {
	return durationOr(c.ReconnectMaxDelay, defaultReconnectMax, "reconnect_max_delay")
}

func (c *Config) endpoint(path string, websocket bool) (string, error) {
	u, err := url.Parse(c.CentralURL)
	if err != nil {
		return "", err
	}
	if websocket {
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else if u.Scheme == "http" {
			u.Scheme = "ws"
		}
	} else {
		if u.Scheme == "wss" {
			u.Scheme = "https"
		} else if u.Scheme == "ws" {
			u.Scheme = "http"
		}
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

// SaveConfig writes credentials atomically. JSON inputs remain JSON; all other
// inputs are written as canonical YAML. On Unix, the final file is mode 0600.
func SaveConfig(path string, c *Config) error {
	original, _ := os.ReadFile(path)
	var data []byte
	var err error
	if json.Valid(original) || strings.EqualFold(filepath.Ext(path), ".json") {
		data, err = json.MarshalIndent(c, "", "  ")
		data = append(data, '\n')
	} else {
		data = marshalSimpleYAML(c)
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// parseSimpleYAML supports mappings of scalar values and scalar lists. It
// deliberately rejects nesting and aliases instead of interpreting surprising
// YAML features in a privileged service configuration.
func parseSimpleYAML(raw []byte) (map[string]any, error) {
	result := make(map[string]any)
	var listKey string
	s := bufio.NewScanner(bytes.NewReader(raw))
	for lineNo := 1; s.Scan(); lineNo++ {
		line := strings.TrimSpace(stripYAMLComment(s.Text()))
		if line == "" || line == "---" {
			continue
		}
		if strings.HasPrefix(line, "-") {
			if listKey == "" {
				return nil, fmt.Errorf("line %d: list item has no key", lineNo)
			}
			item, err := yamlScalar(strings.TrimSpace(strings.TrimPrefix(line, "-")))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("line %d: list items must be strings", lineNo)
			}
			result[listKey] = append(result[listKey].([]string), text)
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			return nil, fmt.Errorf("line %d: expected key: value", lineNo)
		}
		key := strings.TrimSpace(line[:idx])
		if strings.ContainsAny(key, " \t{}[]&*") {
			return nil, fmt.Errorf("line %d: invalid key", lineNo)
		}
		value := strings.TrimSpace(line[idx+1:])
		listKey = ""
		if value == "" {
			listKey = key
			result[key] = []string{}
			continue
		}
		if strings.HasPrefix(value, "[") {
			var items []string
			if err := json.Unmarshal([]byte(value), &items); err != nil {
				return nil, fmt.Errorf("line %d: inline lists must use JSON quoting", lineNo)
			}
			result[key] = items
			continue
		}
		parsed, err := yamlScalar(value)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		result[key] = parsed
	}
	return result, s.Err()
}

func stripYAMLComment(s string) string {
	quote := rune(0)
	for i, r := range s {
		if (r == '\'' || r == '"') && (quote == 0 || quote == r) {
			if quote == 0 {
				quote = r
			} else {
				quote = 0
			}
		}
		if r == '#' && quote == 0 && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}

func yamlScalar(value string) (any, error) {
	if len(value) >= 2 && value[0] == '"' {
		var out string
		if err := json.Unmarshal([]byte(value), &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	switch strings.ToLower(value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "~":
		return nil, nil
	}
	if n, err := strconv.Atoi(value); err == nil {
		return n, nil
	}
	return value, nil
}

func marshalSimpleYAML(c *Config) []byte {
	var b strings.Builder
	write := func(k, v string) { fmt.Fprintf(&b, "%s: %s\n", k, strconv.Quote(v)) }
	write("central_url", c.CentralURL)
	if c.EnrollmentToken != "" {
		write("enrollment_token", c.EnrollmentToken)
	}
	if c.Credential != "" {
		write("credential", c.Credential)
	}
	if c.NodeID != "" {
		write("node_id", c.NodeID)
	}
	write("node_name", c.NodeName)
	b.WriteString("instance_roots:\n")
	for _, root := range c.InstanceRoots {
		fmt.Fprintf(&b, "  - %s\n", strconv.Quote(root))
	}
	write("steamcmd_path", c.SteamCMDPath)
	write("data_dir", c.DataDir)
	if c.HeartbeatInterval != "" {
		write("heartbeat_interval", c.HeartbeatInterval)
	}
	if c.MetricsInterval != "" {
		write("metrics_interval", c.MetricsInterval)
	}
	if c.ReconnectMinDelay != "" {
		write("reconnect_min_delay", c.ReconnectMinDelay)
	}
	if c.ReconnectMaxDelay != "" {
		write("reconnect_max_delay", c.ReconnectMaxDelay)
	}
	fmt.Fprintf(&b, "steamcmd_concurrency: %d\n", c.SteamCMDConcurrency)
	fmt.Fprintf(&b, "insecure_skip_verify: %t\n", c.InsecureSkipVerify)
	return []byte(b.String())
}
