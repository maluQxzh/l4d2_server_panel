package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const maxPublicIPCountryResponse = int64(16 << 10)

var countryCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)

type publicIPCountryEndpoint struct {
	URL   string
	Field string
}

var publicIPCountryEndpoints = []publicIPCountryEndpoint{
	{URL: "https://api.country.is/", Field: "country"},
	{URL: "https://api.ip.sb/geoip", Field: "country_code"},
}

type publicIPCountryResult struct {
	code string
	err  error
}

// shouldPreferOfficialPackageMirrors fails toward mirror-first ordering. The
// official order is retained only when both independent services return the
// same valid non-CN country code for the Agent's public egress address.
func shouldPreferOfficialPackageMirrors(ctx context.Context, client *http.Client) bool {
	lookupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan publicIPCountryResult, len(publicIPCountryEndpoints))
	for _, endpoint := range publicIPCountryEndpoints {
		endpoint := endpoint
		go func() {
			code, err := readPublicIPCountry(lookupCtx, client, endpoint)
			results <- publicIPCountryResult{code: code, err: err}
		}()
	}

	countries := make([]string, 0, len(publicIPCountryEndpoints))
	for range publicIPCountryEndpoints {
		select {
		case <-ctx.Done():
			return true
		case result := <-results:
			if result.err != nil {
				continue
			}
			if result.code == "CN" {
				return true
			}
			countries = append(countries, result.code)
		}
	}
	return len(countries) != len(publicIPCountryEndpoints) || countries[0] != countries[1]
}

func readPublicIPCountry(ctx context.Context, client *http.Client, endpoint publicIPCountryEndpoint) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "l4d2panel-agent/1")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("read public IP country: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("public IP country service returned %s", response.Status)
	}
	if response.ContentLength > maxPublicIPCountryResponse {
		return "", errors.New("public IP country response exceeds 16 KiB")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPublicIPCountryResponse+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > maxPublicIPCountryResponse {
		return "", errors.New("public IP country response exceeds 16 KiB")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decode public IP country response: %w", err)
	}
	code, ok := payload[endpoint.Field].(string)
	code = strings.ToUpper(strings.TrimSpace(code))
	if !ok || !countryCodePattern.MatchString(code) {
		return "", errors.New("public IP country response did not contain a valid country code")
	}
	return code, nil
}

func publicIPCountryHosts() map[string]bool {
	return map[string]bool{
		"api.country.is": true,
		"api.ip.sb":      true,
	}
}
