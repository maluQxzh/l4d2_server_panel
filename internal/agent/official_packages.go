package agent

import (
	"context"
	"crypto/tls"
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

var hrefPattern = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)

type OfficialPackage struct {
	Kind    string
	URL     string
	Version string
	OS      string
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
	return resolveOfficialPackageWithClient(ctx, kind, runtime.GOOS, newRestrictedHTTPClient(officialPageHosts(), 20*time.Second, 2))
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
		return OfficialPackage{Kind: kind, URL: candidate.String(), Version: versionMatch[1], OS: targetOS}, nil
	}
	return OfficialPackage{}, fmt.Errorf("official %s stable page did not contain a trusted %s package", kind, targetOS)
}

func officialPageHosts() map[string]bool {
	return map[string]bool{
		"www.sourcemod.net":     true,
		"sourcemod.net":         true,
		"www.metamodsource.net": true,
		"metamodsource.net":     true,
	}
}

func officialAssetHosts() map[string]bool {
	return map[string]bool{
		"github.com":                           true,
		"release-assets.githubusercontent.com": true,
		"objects.githubusercontent.com":        true,
		"sm.alliedmods.net":                    true,
		"mms.alliedmods.net":                   true,
	}
}

func validateOfficialAssetURL(rawURL string) error {
	if err := validateDownloadURL(rawURL); err != nil {
		return err
	}
	u, _ := url.Parse(rawURL)
	if !officialAssetHosts()[strings.ToLower(u.Hostname())] {
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
