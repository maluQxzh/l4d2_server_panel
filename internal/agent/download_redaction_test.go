package agent

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestURLSecretsAreRedactedFromResultsAndErrors(t *testing.T) {
	raw := "https://user:pass@mods.example/plugin.zip?token=secret#fragment"
	if got := displayHTTPSURL(raw); got != "https://mods.example/plugin.zip" {
		t.Fatalf("display URL = %q", got)
	}
	message := sanitizeJobError(errors.New("request " + raw + " failed"))
	for _, secret := range []string{"user", "pass", "token", "secret", "fragment"} {
		if strings.Contains(message, secret) {
			t.Fatalf("sanitized error leaked %q: %s", secret, message)
		}
	}
}

func TestPublicDownloadIPRejectsMappedPrivateRanges(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"0.0.0.0", "127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254",
		"::", "::1", "fc00::1", "fe80::1",
		"::ffff:0.0.0.0", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:100.64.0.1", "::ffff:169.254.169.254",
	}
	for _, raw := range blocked {
		if publicDownloadIP(netip.MustParseAddr(raw)) {
			t.Errorf("publicDownloadIP(%s) = true, want false", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "::ffff:8.8.8.8", "2606:4700:4700::1111"} {
		if !publicDownloadIP(netip.MustParseAddr(raw)) {
			t.Errorf("publicDownloadIP(%s) = false, want true", raw)
		}
	}
}
