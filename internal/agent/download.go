package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxPackageDownload = int64(512 << 20)

var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")
var embeddedHTTPSURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func DownloadHTTPS(ctx context.Context, rawURL, destination string) (string, int64, error) {
	return downloadHTTPS(ctx, rawURL, destination, validateDownloadURL)
}

func DownloadOfficialHTTPS(ctx context.Context, rawURL, destination string) (string, int64, error) {
	return downloadHTTPS(ctx, rawURL, destination, validateOfficialAssetURL)
}

func downloadPackageFromSources(ctx context.Context, sources []string, destination string, official bool,
	downloader func(context.Context, string, string, bool) (string, int64, error), progress ProgressFunc,
) (string, int64, string, error) {
	var failures []error
	for index, source := range sources {
		if official {
			if err := validateOfficialAssetURL(source); err != nil {
				failures = append(failures, err)
				continue
			}
			if index > 0 {
				progress(10, "retrying official package from trusted AlliedModders fallback")
			}
		}
		hash, size, err := downloader(ctx, source, destination, official)
		if err == nil {
			return hash, size, source, nil
		}
		parsed, _ := url.Parse(source)
		failures = append(failures, fmt.Errorf("%s: %w", parsed.Hostname(), err))
		if ctx.Err() != nil {
			return "", 0, "", ctx.Err()
		}
	}
	if official {
		return "", 0, "", fmt.Errorf("download official package from trusted sources failed: %w", errors.Join(failures...))
	}
	return "", 0, "", errors.Join(failures...)
}

func downloadHTTPS(ctx context.Context, rawURL, destination string, validateURL func(string) error) (string, int64, error) {
	if err := validateURL(rawURL); err != nil {
		return "", 0, err
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(dialCtx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !publicDownloadIP(ip) {
					continue
				}
				return dialer.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), port))
			}
			return nil, errors.New("download host resolves only to blocked network addresses")
		},
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("download exceeded three redirects")
		}
		return validateURL(req.URL.String())
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	request.Header.Set("User-Agent", "l4d2panel-agent/1")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		parsed, _ := url.Parse(rawURL)
		return "", 0, fmt.Errorf("download package from %s failed", parsed.Hostname())
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, fmt.Errorf("package server returned %s", response.Status)
	}
	if response.ContentLength > maxPackageDownload {
		return "", 0, errors.New("package exceeds 512 MiB download limit")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
	if err != nil {
		return "", 0, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o600)
	hash := sha256.New()
	limited := io.LimitReader(response.Body, maxPackageDownload+1)
	written, err := io.Copy(io.MultiWriter(tmp, hash), limited)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	if written > maxPackageDownload {
		return "", 0, errors.New("package exceeds 512 MiB download limit")
	}
	if err := os.Rename(name, destination); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func displayHTTPSURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "https") {
		return raw
	}
	parsed.RawQuery, parsed.Fragment, parsed.User = "", "", nil
	return parsed.String()
}

func redactEmbeddedURLSecrets(value string) string {
	return embeddedHTTPSURL.ReplaceAllStringFunc(value, func(match string) string {
		trailing := ""
		for len(match) > 0 && strings.ContainsRune(".,;:)]}", rune(match[len(match)-1])) {
			trailing = match[len(match)-1:] + trailing
			match = match[:len(match)-1]
		}
		return displayHTTPSURL(match) + trailing
	})
}

func validateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return errors.New("package source must be an absolute HTTPS URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New("package downloads require HTTPS")
	}
	if u.User != nil {
		return errors.New("credentials in package URLs are not allowed")
	}
	if u.Fragment != "" {
		return errors.New("package URL fragments are not allowed")
	}
	return nil
}

func publicDownloadIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip.Is4() && carrierGradeNAT.Contains(ip) {
		return false
	}
	return true
}
