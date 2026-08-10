package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const maxOfficialDownloadPage = int64(2 << 20)

const maxGitHubReleaseMetadata = int64(4 << 20)

var hrefPattern = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
var officialVersionBranchPattern = regexp.MustCompile(`^(\d+\.\d+)(?:\.|$)`)
var githubAssetDigestPattern = regexp.MustCompile(`(?i)^sha256:([0-9a-f]{64})$`)

var officialGitHubMirrorPrefixes = []string{
	"https://gh-proxy.com/",
	"https://ghfast.top/",
	"https://ghproxy.net/",
}

type OfficialPackage struct {
	Kind           string
	URL            string
	FallbackURLs   []string
	MirrorURLs     []string
	PreferMirrors  bool
	ExpectedSHA256 string
	ExpectedSize   int64
	Version        string
	OS             string
}

type officialPackageSpec struct {
	pageURL       string
	repository    string
	filePrefix    string
	legacyHost    string
	legacyPrefix  string
	versionRegexp *regexp.Regexp
}

var officialPackageSpecs = map[string]officialPackageSpec{
	"sourcemod": {
		pageURL:       "https://www.sourcemod.net/downloads.php?branch=stable",
		repository:    "/alliedmodders/sourcemod/releases/download/",
		filePrefix:    "sourcemod-",
		legacyHost:    "sm.alliedmods.net",
		legacyPrefix:  "/smdrop/",
		versionRegexp: regexp.MustCompile(`(?i)^sourcemod-(.+)-(?:windows\.zip|linux\.tar\.gz)$`),
	},
	"metamod": {
		pageURL:       "https://www.metamodsource.net/downloads.php?branch=stable",
		repository:    "/alliedmodders/metamod-source/releases/download/",
		filePrefix:    "mmsource-",
		legacyHost:    "mms.alliedmods.net",
		legacyPrefix:  "/mmsdrop/",
		versionRegexp: regexp.MustCompile(`(?i)^mmsource-(.+)-(?:windows\.zip|linux\.tar\.gz)$`),
	},
}

// ResolveOfficialPackage reads the AlliedModders stable-build page and returns
// the package selected by that page for the current Agent operating system.
// The HTML is intentionally treated as untrusted input: only known AlliedModders
// repositories and artifact hosts can be selected.
func ResolveOfficialPackage(ctx context.Context, kind string) (OfficialPackage, error) {
	resolved, err := resolveOfficialPackageWithClient(ctx, kind, runtime.GOOS, newRestrictedHTTPClient(officialPageHosts(), 20*time.Second, 2))
	if err != nil {
		return OfficialPackage{}, err
	}
	integrityResult := make(chan OfficialPackage, 1)
	go func() {
		integrityResult <- addVerifiedGitHubMirrors(ctx, resolved, newRestrictedHTTPClient(githubAPIHosts(), 20*time.Second, 0))
	}()
	preferMirrors := shouldPreferOfficialPackageMirrors(ctx, newRestrictedHTTPClient(publicIPCountryHosts(), 4*time.Second, 0))
	resolved = <-integrityResult
	resolved.PreferMirrors = preferMirrors && len(resolved.MirrorURLs) > 0
	return resolved, nil
}

func officialPackageDownloadSources(resolved OfficialPackage) []string {
	capacity := 1 + len(resolved.FallbackURLs) + len(resolved.MirrorURLs)
	result := make([]string, 0, capacity)
	if resolved.PreferMirrors {
		result = append(result, resolved.MirrorURLs...)
	}
	result = append(result, resolved.URL)
	result = append(result, resolved.FallbackURLs...)
	if !resolved.PreferMirrors {
		result = append(result, resolved.MirrorURLs...)
	}
	return result
}

func addVerifiedGitHubMirrors(ctx context.Context, resolved OfficialPackage, client *http.Client) OfficialPackage {
	digest, size, err := resolveGitHubAssetIntegrity(ctx, resolved.URL, client)
	if err != nil {
		return resolved
	}
	resolved.ExpectedSHA256 = digest
	resolved.ExpectedSize = size
	resolved.MirrorURLs = officialGitHubMirrorURLs(resolved.URL)
	return resolved
}

func resolveGitHubAssetIntegrity(ctx context.Context, assetURL string, client *http.Client) (string, int64, error) {
	owner, repository, tag, fileName, err := parseAlliedModdersGitHubAssetURL(assetURL)
	if err != nil {
		return "", 0, err
	}
	metadataURL := (&url.URL{
		Scheme: "https",
		Host:   "api.github.com",
		Path:   path.Join("/repos", owner, repository, "releases/tags", tag),
	}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return "", 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "l4d2panel-agent/1")
	response, err := client.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("read GitHub release metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, fmt.Errorf("GitHub release metadata returned %s", response.Status)
	}
	if response.ContentLength > maxGitHubReleaseMetadata {
		return "", 0, errors.New("GitHub release metadata exceeds 4 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubReleaseMetadata+1))
	if err != nil {
		return "", 0, err
	}
	if int64(len(raw)) > maxGitHubReleaseMetadata {
		return "", 0, errors.New("GitHub release metadata exceeds 4 MiB")
	}
	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			State              string `json:"state"`
			Digest             string `json:"digest"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &release); err != nil {
		return "", 0, fmt.Errorf("decode GitHub release metadata: %w", err)
	}
	for _, asset := range release.Assets {
		if asset.Name != fileName || asset.State != "uploaded" || asset.BrowserDownloadURL != assetURL || asset.Size <= 0 || asset.Size > maxPackageDownload {
			continue
		}
		match := githubAssetDigestPattern.FindStringSubmatch(strings.TrimSpace(asset.Digest))
		if len(match) != 2 {
			continue
		}
		return strings.ToLower(match[1]), asset.Size, nil
	}
	return "", 0, errors.New("GitHub release asset did not provide a trusted SHA-256 digest")
}

func resolveOfficialPackageWithClient(ctx context.Context, kind, targetOS string, client *http.Client) (OfficialPackage, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	spec, ok := officialPackageSpecs[kind]
	if !ok {
		return OfficialPackage{}, fmt.Errorf("unsupported official package %q", kind)
	}
	if targetOS != "windows" && targetOS != "linux" {
		return OfficialPackage{}, fmt.Errorf("official %s packages are unavailable for %s", kind, targetOS)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.pageURL, nil)
	if err != nil {
		return OfficialPackage{}, err
	}
	request.Header.Set("User-Agent", "l4d2panel-agent/1")
	response, err := client.Do(request)
	if err != nil {
		return OfficialPackage{}, fmt.Errorf("read official %s stable download page: %w", kind, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return OfficialPackage{}, fmt.Errorf("official %s stable download page returned %s", kind, response.Status)
	}
	if response.ContentLength > maxOfficialDownloadPage {
		return OfficialPackage{}, errors.New("official stable download page exceeds 2 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxOfficialDownloadPage+1))
	if err != nil {
		return OfficialPackage{}, err
	}
	if int64(len(raw)) > maxOfficialDownloadPage {
		return OfficialPackage{}, errors.New("official stable download page exceeds 2 MiB")
	}
	return parseOfficialPackagePage(kind, targetOS, spec.pageURL, raw)
}

func parseOfficialPackagePage(kind, targetOS, pageURL string, raw []byte) (OfficialPackage, error) {
	spec, ok := officialPackageSpecs[kind]
	if !ok {
		return OfficialPackage{}, fmt.Errorf("unsupported official package %q", kind)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return OfficialPackage{}, err
	}
	wantedSuffix := "-linux.tar.gz"
	if targetOS == "windows" {
		wantedSuffix = "-windows.zip"
	} else if targetOS != "linux" {
		return OfficialPackage{}, fmt.Errorf("unsupported package operating system %q", targetOS)
	}
	for _, match := range hrefPattern.FindAllSubmatch(raw, -1) {
		href := html.UnescapeString(strings.TrimSpace(string(match[1])))
		candidate, err := base.Parse(href)
		if err != nil || !strings.EqualFold(candidate.Scheme, "https") || candidate.User != nil || candidate.Fragment != "" {
			continue
		}
		host := strings.ToLower(candidate.Hostname())
		decodedPath, unescapeErr := url.PathUnescape(candidate.EscapedPath())
		if unescapeErr != nil {
			continue
		}
		cleanPath := path.Clean(decodedPath)
		validPath := host == "github.com" && strings.HasPrefix(strings.ToLower(cleanPath), spec.repository)
		validPath = validPath || host == spec.legacyHost && strings.HasPrefix(strings.ToLower(cleanPath), spec.legacyPrefix)
		fileName := path.Base(cleanPath)
		if !validPath || !strings.HasPrefix(strings.ToLower(fileName), spec.filePrefix) || !strings.HasSuffix(strings.ToLower(fileName), wantedSuffix) {
			continue
		}
		versionMatch := spec.versionRegexp.FindStringSubmatch(fileName)
		if len(versionMatch) != 2 {
			continue
		}
		result := OfficialPackage{Kind: kind, URL: candidate.String(), Version: versionMatch[1], OS: targetOS}
		if host == "github.com" {
			result.FallbackURLs = officialLegacyFallbackURLs(spec, versionMatch[1], fileName)
		}
		return result, nil
	}
	return OfficialPackage{}, fmt.Errorf("official %s stable page did not contain a trusted %s package", kind, targetOS)
}

func officialLegacyFallbackURLs(spec officialPackageSpec, version, fileName string) []string {
	branch := officialVersionBranchPattern.FindStringSubmatch(version)
	if len(branch) != 2 || spec.legacyHost == "" || spec.legacyPrefix == "" {
		return nil
	}
	fallback := (&url.URL{Scheme: "https", Host: spec.legacyHost, Path: path.Join(spec.legacyPrefix, branch[1], fileName)}).String()
	if err := validateOfficialAssetURL(fallback); err != nil {
		return nil
	}
	return []string{fallback}
}

func officialGitHubMirrorURLs(assetURL string) []string {
	if _, _, _, _, err := parseAlliedModdersGitHubAssetURL(assetURL); err != nil {
		return nil
	}
	result := make([]string, 0, len(officialGitHubMirrorPrefixes))
	for _, prefix := range officialGitHubMirrorPrefixes {
		candidate := prefix + assetURL
		if err := validateOfficialAssetURL(candidate); err == nil {
			result = append(result, candidate)
		}
	}
	return result
}

func parseAlliedModdersGitHubAssetURL(rawURL string) (string, string, string, string, error) {
	if err := validateDownloadURL(rawURL); err != nil {
		return "", "", "", "", err
	}
	u, _ := url.Parse(rawURL)
	if !strings.EqualFold(u.Hostname(), "github.com") || u.RawQuery != "" {
		return "", "", "", "", errors.New("official GitHub package URL is invalid")
	}
	decodedPath, err := url.PathUnescape(u.EscapedPath())
	if err != nil || path.Clean(decodedPath) != decodedPath {
		return "", "", "", "", errors.New("official GitHub package path is invalid")
	}
	parts := strings.Split(strings.TrimPrefix(decodedPath, "/"), "/")
	if len(parts) != 6 || parts[0] != "alliedmodders" || parts[2] != "releases" || parts[3] != "download" || parts[4] == "" || parts[5] == "" {
		return "", "", "", "", errors.New("official GitHub package path is invalid")
	}
	if parts[1] != "sourcemod" && parts[1] != "metamod-source" {
		return "", "", "", "", errors.New("official GitHub package repository is invalid")
	}
	return parts[0], parts[1], parts[4], parts[5], nil
}

func validateOfficialMirrorURL(rawURL string) error {
	if err := validateDownloadURL(rawURL); err != nil {
		return err
	}
	u, _ := url.Parse(rawURL)
	if !officialMirrorHosts()[strings.ToLower(u.Hostname())] || u.RawQuery != "" {
		return errors.New("official package mirror URL is invalid")
	}
	const embeddedPrefix = "/https://github.com/"
	if !strings.HasPrefix(u.Path, embeddedPrefix) {
		return errors.New("official package mirror target is invalid")
	}
	embedded := strings.TrimPrefix(u.Path, "/")
	if _, _, _, _, err := parseAlliedModdersGitHubAssetURL(embedded); err != nil {
		return errors.New("official package mirror target is invalid")
	}
	return nil
}

func officialPageHosts() map[string]bool {
	return map[string]bool{
		"www.sourcemod.net":     true,
		"sourcemod.net":         true,
		"www.metamodsource.net": true,
		"metamodsource.net":     true,
	}
}

func githubAPIHosts() map[string]bool {
	return map[string]bool{"api.github.com": true}
}

func officialMirrorHosts() map[string]bool {
	return map[string]bool{
		"gh-proxy.com": true,
		"ghfast.top":   true,
		"ghproxy.net":  true,
	}
}

func officialAssetHosts() map[string]bool {
	return map[string]bool{
		"github.com":                           true,
		"release-assets.githubusercontent.com": true,
		"objects.githubusercontent.com":        true,
		"steamcdn-a.akamaihd.net":              true,
		"sm.alliedmods.net":                    true,
		"mms.alliedmods.net":                   true,
		"gh-proxy.com":                         true,
		"ghfast.top":                           true,
		"ghproxy.net":                          true,
	}
}

func validateOfficialAssetURL(rawURL string) error {
	if err := validateDownloadURL(rawURL); err != nil {
		return err
	}
	u, _ := url.Parse(rawURL)
	host := strings.ToLower(u.Hostname())
	if officialMirrorHosts()[host] {
		return validateOfficialMirrorURL(rawURL)
	}
	if !officialAssetHosts()[host] {
		return errors.New("official package redirected to an untrusted host")
	}
	return nil
}

func newRestrictedHTTPClient(allowedHosts map[string]bool, timeout time.Duration, maxRedirects int) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 20 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       20 * time.Second,
		DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if !allowedHosts[strings.ToLower(host)] {
				return nil, errors.New("request host is not allowlisted")
			}
			ips, err := net.DefaultResolver.LookupNetIP(dialCtx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if publicDownloadIP(ip) {
					return dialer.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), port))
				}
			}
			return nil, errors.New("request host resolves only to blocked network addresses")
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("request exceeded redirect limit")
			}
			if err := validateDownloadURL(request.URL.String()); err != nil {
				return err
			}
			if !allowedHosts[strings.ToLower(request.URL.Hostname())] {
				return errors.New("request redirected to an untrusted host")
			}
			return nil
		},
	}
}
