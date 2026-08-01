package control

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestValidatePublicAddress(t *testing.T) {
	t.Parallel()
	valid := []string{"", "play.example.com", "203.0.113.20", "[2001:db8::20]"}
	for _, value := range valid {
		if err := validatePublicAddress(value); err != nil {
			t.Errorf("validatePublicAddress(%q): %v", value, err)
		}
	}
	invalid := []string{"https://example.com", "example.com:27015", "2001:db8::20", "bad..host", "user@example.com"}
	for _, value := range invalid {
		if err := validatePublicAddress(value); err == nil {
			t.Errorf("validatePublicAddress(%q) unexpectedly succeeded", value)
		}
	}
}

func TestValidateNotificationChannel(t *testing.T) {
	t.Parallel()
	webhook := map[string]any{"url": "https://hooks.example.com/l4d2"}
	if err := validateChannel("webhook", webhook, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := validateChannel("webhook", map[string]any{"url": "http://127.0.0.1/hook"}, "0123456789abcdef"); err == nil {
		t.Fatal("insecure webhook URL was accepted")
	}
	if err := validateChannel("webhook", map[string]any{"url": "https://127.0.0.1/hook"}, "0123456789abcdef"); err == nil {
		t.Fatal("private webhook address was accepted")
	}
	if err := validateChannel("webhook", map[string]any{"url": "https://hooks.example.com/l4d2?token=plain"}, "0123456789abcdef"); err == nil {
		t.Fatal("webhook URL with a plaintext query secret was accepted")
	}
	if err := validateChannel("webhook", map[string]any{"url": "https://hooks.example.com/l4d2", "token": "plain"}, "0123456789abcdef"); err == nil {
		t.Fatal("plaintext config secret was accepted")
	}
	smtpConfig := map[string]any{"host": "smtp.example.com", "from": "panel@example.com", "to": []any{"ops@example.com"}}
	if err := validateChannel("smtp", smtpConfig, "password"); err != nil {
		t.Fatal(err)
	}
	if smtpConfig["port"] != 587 {
		t.Fatalf("default SMTP port = %v, want 587", smtpConfig["port"])
	}
}

func TestPublicWebhookIP(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"0.0.0.0", "127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.168.1.1",
		"::", "::1", "fc00::1", "fe80::1",
		"::ffff:0.0.0.0", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:100.64.0.1", "::ffff:169.254.169.254",
	}
	for _, raw := range blocked {
		if publicWebhookIP(netip.MustParseAddr(raw)) {
			t.Errorf("publicWebhookIP(%s) = true, want false", raw)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "::ffff:8.8.8.8", "2606:4700:4700::1111"}
	for _, raw := range allowed {
		if !publicWebhookIP(netip.MustParseAddr(raw)) {
			t.Errorf("publicWebhookIP(%s) = false, want true", raw)
		}
	}
}

func TestWebhookSignature(t *testing.T) {
	t.Parallel()
	got := webhookSignature("secret", "1700000000", []byte(`{"ok":true}`))
	want := "sha256=c1afc7c2df3db0690d7d75954610ed1a1d959ce96355ccb8c0a8bc09fd0cfc27"
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestRetryDelayCaps(t *testing.T) {
	t.Parallel()
	if got := retryDelay(1); got != 30*time.Second {
		t.Fatalf("first retry = %s", got)
	}
	if got := retryDelay(99); got != 6*time.Hour {
		t.Fatalf("capped retry = %s", got)
	}
}

func TestParseMetricQuery(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	values := url.Values{"node_id": {"d0f2bc53-b66d-4e3b-9f98-c04de97be58f"}, "resolution": {"1h"}}
	query, err := parseMetricQuery(values, now)
	if err != nil {
		t.Fatal(err)
	}
	if query.To.Sub(query.From) != 30*24*time.Hour {
		t.Fatalf("default window = %s", query.To.Sub(query.From))
	}
	values.Set("from", now.Add(-367*24*time.Hour).Format(time.RFC3339))
	if _, err := parseMetricQuery(values, now); err == nil {
		t.Fatal("oversized metrics range was accepted")
	}
}

func TestNotificationPayloadDoesNotExposeChannelSecret(t *testing.T) {
	t.Parallel()
	channel := NotificationChannel{ID: "1", Name: "ops", Kind: "webhook", HasSecret: true, Config: map[string]any{"url": "https://example.com"}}
	raw, err := json.Marshal(channel)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || json.Valid(raw) == false {
		t.Fatal("invalid JSON")
	}
	if _, ok := channel.Config["secret"]; ok {
		t.Fatal("secret exposed in public channel config")
	}
}
