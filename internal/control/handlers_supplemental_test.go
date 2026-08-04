package control

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestNormalizeQQFlashShareURL(t *testing.T) {
	input := "测试用户通过QQ闪传分享了文件\n链接：[https://qfile.qq.com/q/Example123](https://qfile.qq.com/q/Example123)"
	if got, err := normalizeQQFlashShareURL(input); err != nil || got != "https://qfile.qq.com/q/Example123" {
		t.Fatalf("normalized share URL = %q, %v", got, err)
	}
	for _, value := range []string{"https://evil.example/q/Example123", "https://qfile.qq.com.evil.example/q/Example123",
		"https://qfile.qq.com/q/" + strings.Repeat("a", 65),
		"https://qfile.qq.com/q/first01 https://qfile.qq.com/q/second02"} {
		if _, err := normalizeQQFlashShareURL(value); err == nil {
			t.Fatalf("unsafe QQ share input accepted: %q", value)
		}
	}
}

func TestSafeBroadcastCommand(t *testing.T) {
	command, err := safeBroadcastCommand("Server restarts in five minutes")
	if err != nil {
		t.Fatal(err)
	}
	if command != `say "Server restarts in five minutes"` {
		t.Fatalf("unexpected command %q", command)
	}
	for _, value := range []string{"", "hello; quit", "line\nbreak", `quote"break`} {
		if _, err := safeBroadcastCommand(value); err == nil {
			t.Errorf("expected %q to be rejected", value)
		}
	}
}

func TestValidatePackageUpload(t *testing.T) {
	valid := map[string]any{"name": "campaign.vpk", "content_base64": base64.StdEncoding.EncodeToString([]byte("vpk"))}
	if err := validatePackageUpload(valid); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []map[string]any{
		{"name": "../plugin.smx", "content_base64": "YQ=="},
		{"name": "plugin.smx", "source": "https://example.test/p.smx", "content_base64": "YQ=="},
		{"name": "plugin.smx", "content_base64": "not-base64"},
	} {
		if err := validatePackageUpload(payload); err == nil {
			t.Fatalf("unsafe upload was accepted: %#v", payload)
		}
	}
}

func TestParseConnectedDuration(t *testing.T) {
	if value, ok := parseConnectedDuration("01:23"); !ok || value != time.Minute+23*time.Second {
		t.Fatalf("unexpected duration %v, %v", value, ok)
	}
	if value, ok := parseConnectedDuration("02:01:23"); !ok || value != 2*time.Hour+time.Minute+23*time.Second {
		t.Fatalf("unexpected duration %v, %v", value, ok)
	}
	if _, ok := parseConnectedDuration("bad; quit"); ok {
		t.Fatal("invalid connected duration was accepted")
	}
}

func TestRCONPlayerRefreshRequiresRunningServerAndCredential(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.InstanceStatus{domain.InstanceRunning, domain.InstanceDegraded} {
		if !rconPlayerRefreshAllowed(domain.Instance{Status: status, RCONConfigured: true}) {
			t.Fatalf("RCON refresh rejected status %s", status)
		}
	}
	for _, instance := range []domain.Instance{
		{Status: domain.InstanceStopped, RCONConfigured: true},
		{Status: domain.InstanceRunning, RCONConfigured: false},
		{Status: domain.InstanceExternal, RCONConfigured: true},
	} {
		if rconPlayerRefreshAllowed(instance) {
			t.Fatalf("RCON refresh accepted instance %#v", instance)
		}
	}
}

func TestPackageNameDoesNotKeepSignedURLQuery(t *testing.T) {
	if got := packageNameFromSource("https://mods.example/plugin.zip?token=secret#fragment"); got != "plugin.zip" {
		t.Fatalf("packageNameFromSource() = %q", got)
	}
}

func TestNormalizePlayerBanSupportsTimedAndPermanentBans(t *testing.T) {
	command, err := normalizePlayerCommand("ban_player", map[string]any{"steam_id": "STEAM_1:1:12345", "duration_minutes": 60, "reason": "spam"})
	if err != nil {
		t.Fatal(err)
	}
	if command != "banid 60 STEAM_1:1:12345 kick; writeid" {
		t.Fatalf("unexpected command %q", command)
	}
	for _, payload := range []map[string]any{
		{"steam_id": "bad;quit"},
		{"steam_id": "STEAM_1:1:12345", "duration_minutes": -1},
		{"steam_id": "STEAM_1:1:12345", "duration_minutes": 525601},
		{"steam_id": "STEAM_1:1:12345", "reason": "line\nbreak"},
	} {
		if _, err := normalizePlayerCommand("ban_player", payload); err == nil {
			t.Fatalf("unsafe ban payload was accepted: %#v", payload)
		}
	}
}
