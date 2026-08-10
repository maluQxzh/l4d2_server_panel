package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type agentRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn agentRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestParseOfficialPackagePageSelectsTrustedOperatingSystemAsset(t *testing.T) {
	html := []byte(`
<a href="https://github.com/attacker/sourcemod/releases/download/9/evil-windows.zip">Windows</a>
<a href="https://github.com/alliedmodders/sourcemod/releases/download/1.12.0.7246/sourcemod-1.12.0-git7246-windows.zip">Windows</a>
<a href="https://github.com/alliedmodders/sourcemod/releases/download/1.12.0.7246/sourcemod-1.12.0-git7246-linux.tar.gz">Linux</a>`)
	windows, err := parseOfficialPackagePage("sourcemod", "windows", officialPackageSpecs["sourcemod"].pageURL, html)
	if err != nil {
		t.Fatal(err)
	}
	if windows.Version != "1.12.0-git7246" || !strings.HasSuffix(windows.URL, "-windows.zip") {
		t.Fatalf("unexpected Windows package: %#v", windows)
	}
	if len(windows.FallbackURLs) != 1 || windows.FallbackURLs[0] != "https://sm.alliedmods.net/smdrop/1.12/sourcemod-1.12.0-git7246-windows.zip" {
		t.Fatalf("unexpected Windows fallback package: %#v", windows.FallbackURLs)
	}
	linux, err := parseOfficialPackagePage("sourcemod", "linux", officialPackageSpecs["sourcemod"].pageURL, html)
	if err != nil {
		t.Fatal(err)
	}
	if linux.Version != "1.12.0-git7246" || !strings.HasSuffix(linux.URL, "-linux.tar.gz") {
		t.Fatalf("unexpected Linux package: %#v", linux)
	}
}

func TestOfficialPackageDownloadRetriesTrustedAlliedModdersFallback(t *testing.T) {
	sources := []string{
		"https://github.com/alliedmodders/sourcemod/releases/download/1.12.0.7246/sourcemod-1.12.0-git7246-windows.zip",
		"https://sm.alliedmods.net/smdrop/1.12/sourcemod-1.12.0-git7246-windows.zip",
	}
	var attempted []string
	var progressMessages []string
	hash, size, selected, err := downloadPackageFromSources(context.Background(), sources, t.TempDir()+"/package.zip", true,
		func(_ context.Context, source, _ string, official bool) (string, int64, error) {
			if !official {
				t.Fatal("official fallback was downloaded as an ordinary URL")
			}
			attempted = append(attempted, source)
			if len(attempted) == 1 {
				return "", 0, errors.New("download package from github.com failed")
			}
			return "sha256", 42, nil
		}, func(_ int, message string) { progressMessages = append(progressMessages, message) })
	if err != nil || hash != "sha256" || size != 42 || selected != sources[1] {
		t.Fatalf("fallback download = hash %q size %d selected %q err %v", hash, size, selected, err)
	}
	if !reflect.DeepEqual(attempted, sources) || len(progressMessages) != 1 || !strings.Contains(progressMessages[0], "AlliedModders") {
		t.Fatalf("fallback attempts = %#v, progress = %#v", attempted, progressMessages)
	}
}

func TestParseOfficialPackagePageAcceptsLegacyAlliedModdersDrop(t *testing.T) {
	raw := []byte(`<a href="https://mms.alliedmods.net/mmsdrop/1.12/mmsource-1.12.0-git1225-linux.tar.gz">Linux</a>`)
	got, err := parseOfficialPackagePage("metamod", "linux", officialPackageSpecs["metamod"].pageURL, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.12.0-git1225" {
		t.Fatalf("unexpected version %q", got.Version)
	}
}

func TestParseOfficialPackagePageRejectsUntrustedOrInsecureAsset(t *testing.T) {
	tests := [][]byte{
		[]byte(`<a href="http://github.com/alliedmodders/sourcemod/releases/download/1/sourcemod-1-windows.zip">bad</a>`),
		[]byte(`<a href="https://github.com/other/sourcemod/releases/download/1/sourcemod-1-windows.zip">bad</a>`),
		[]byte(`<a href="https://evil.example/alliedmodders/sourcemod/releases/download/1/sourcemod-1-windows.zip">bad</a>`),
		[]byte(`<a href="https://github.com/alliedmodders/sourcemod/releases/download/%2e%2e/%2e%2e/evil/sourcemod-1-windows.zip">bad</a>`),
	}
	for _, raw := range tests {
		if _, err := parseOfficialPackagePage("sourcemod", "windows", officialPackageSpecs["sourcemod"].pageURL, raw); err == nil {
			t.Fatalf("expected untrusted page link to fail: %s", raw)
		}
	}
}

func TestResolveOfficialPackageLimitsPageAndUsesStableURL(t *testing.T) {
	client := &http.Client{Transport: agentRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != officialPackageSpecs["metamod"].pageURL {
			t.Fatalf("unexpected stable page URL %s", request.URL)
		}
		body := `<a href="https://github.com/alliedmodders/metamod-source/releases/download/1.12.0.1225/mmsource-1.12.0-git1225-windows.zip">Windows</a>`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	got, err := resolveOfficialPackageWithClient(context.Background(), "metamod", "windows", client)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "metamod" || got.OS != "windows" || got.Version != "1.12.0-git1225" {
		t.Fatalf("unexpected resolved package: %#v", got)
	}
}

func TestOfficialAssetURLAllowlist(t *testing.T) {
	for _, allowed := range []string{
		"https://github.com/alliedmodders/sourcemod/releases/download/1/file.zip",
		"https://release-assets.githubusercontent.com/github-production-release-asset/file?token=x",
		"https://sm.alliedmods.net/smdrop/1/file.tar.gz",
	} {
		if err := validateOfficialAssetURL(allowed); err != nil {
			t.Fatalf("expected %s to be allowed: %v", allowed, err)
		}
	}
	for _, blocked := range []string{"http://github.com/file", "https://github.evil.example/file", "https://127.0.0.1/file"} {
		if err := validateOfficialAssetURL(blocked); err == nil {
			t.Fatalf("expected %s to be blocked", blocked)
		}
	}
}
