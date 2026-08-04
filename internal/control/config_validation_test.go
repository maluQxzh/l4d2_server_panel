package control

import (
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestValidateInstanceConfigAcceptsStructuredDefaults(t *testing.T) {
	t.Parallel()
	config := domain.InstanceConfig{
		Hostname: "专家战役 #1", Mode: "coop", Difficulty: "normal", MaxPlayers: 8,
		StartMap: "c1m1_hotel", Region: "255", SteamGroupID: "12345678901234567890",
		ExtraArgs: []string{"-insecure", "-tickrate", "100", "+sv_lan", "0", "+sv_tags=coop campaign"}, CVars: map[string]string{"sv_consistency": "1"},
		MaintenanceCron: "0 4 * * *",
	}
	if err := validateStructuredConfig(config); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateInstanceConfigRejectsAgentIncompatibleValues(t *testing.T) {
	t.Parallel()
	tests := []domain.InstanceConfig{
		{MaxPlayers: 33},
		{StartMap: "../c1m1_hotel"},
		{Mode: "coop;quit"},
		{Difficulty: "nightmare"},
		{SteamGroupID: "12-not-digits"},
		{ServerPassword: "safe\nquit"},
		{ServerPassword: strings.Repeat("a", 257)},
		{ExtraArgs: []string{"-tickrate"}},
		{ExtraArgs: []string{"-tickrate", "0"}},
		{ExtraArgs: []string{"-tickrate=1001"}},
		{ExtraArgs: []string{"-tickrate", "fast"}},
		{ExtraArgs: []string{"-tickrate", "100", "-tickrate", "60"}},
		{ExtraArgs: []string{"+sv_password", "plaintext"}},
		{ExtraArgs: []string{"+rcon_password plaintext"}},
		{ExtraArgs: []string{"+z_difficulty hard"}},
		{ExtraArgs: []string{"+sv_password\tplaintext"}},
		{ExtraArgs: []string{"+plugin_load addons/evil"}},
		{ExtraArgs: []string{"-port 27016"}},
		{ExtraArgs: []string{"-basedir=C:/escape"}},
		{ExtraArgs: []string{`-basedir C:\escape`}},
		{ExtraArgs: []string{"-defaultgamedir other"}},
		{ExtraArgs: []string{"-usetcp"}},
		{ExtraArgs: []string{" \t "}},
		{CVars: map[string]string{"rcon_password": "plaintext"}},
		{CVars: map[string]string{"exec": "safe\nquit"}},
		{MaintenanceCron: "0 0 4 * * *"},
		{MaintenanceCron: "not a cron expression"},
	}
	for index, config := range tests {
		if err := validateStructuredConfig(config); err == nil {
			t.Fatalf("case %d was accepted: %#v", index, config)
		}
	}
}

func TestValidateRCONPasswordAllowsBlankAndRejectsUnsafeText(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "safe password"} {
		if err := validateRCONPassword(value); err != nil {
			t.Fatalf("valid RCON password rejected: %v", err)
		}
	}
	for _, value := range []string{"bad\rpassword", "bad\x00password", strings.Repeat("a", 257)} {
		if err := validateRCONPassword(value); err == nil {
			t.Fatalf("unsafe RCON password was accepted: %q", value)
		}
	}
}

func TestDiscardRetiredInstanceConfigFields(t *testing.T) {
	t.Parallel()
	config := domain.InstanceConfig{
		Hostname: "legacy client", ServerPassword: "old-password", Region: "255",
		Mode: "coop", Difficulty: "normal", MaxPlayers: 4, StartMap: "c1m1_hotel",
	}
	discardRetiredInstanceConfigFields(&config)
	if config.ServerPassword != "" || config.Region != "" {
		t.Fatalf("retired fields were preserved: %#v", config)
	}
	if err := validateStructuredConfig(config); err != nil {
		t.Fatalf("sanitized legacy config was rejected: %v", err)
	}
}

func TestValidateServerPasswordAppliesToTopLevelPatchSecret(t *testing.T) {
	t.Parallel()
	if err := validateServerPassword("safe password"); err != nil {
		t.Fatalf("valid top-level password rejected: %v", err)
	}
	for _, value := range []string{"bad\rpassword", "bad\x00password", strings.Repeat("a", 257)} {
		if err := validateServerPassword(value); err == nil {
			t.Fatalf("unsafe top-level password was accepted: %q", value)
		}
	}
}

func TestValidInstanceAgentHostOnlyAllowsLocalBindAddresses(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"", "0.0.0.0", "127.0.0.1", "::", "::1", "[::1]"} {
		if !validInstanceAgentHost(host) {
			t.Fatalf("local host %q was rejected", host)
		}
	}
	for _, host := range []string{"localhost", "192.168.1.4", "169.254.169.254", "example.com"} {
		if validInstanceAgentHost(host) {
			t.Fatalf("non-numeric or remote host %q was accepted", host)
		}
	}
}
