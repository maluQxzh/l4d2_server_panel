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

type publicIPCountryTestResponse struct {
	body string
	err  error
}

func TestShouldPreferOfficialPackageMirrors(t *testing.T) {
	tests := []struct {
		name          string
		countryIS     publicIPCountryTestResponse
		ipSB          publicIPCountryTestResponse
		preferMirrors bool
	}{
		{name: "mainland China", countryIS: publicIPCountryTestResponse{body: `{"country":"CN"}`}, ipSB: publicIPCountryTestResponse{body: `{"country_code":"CN"}`}, preferMirrors: true},
		{name: "agreed outside mainland", countryIS: publicIPCountryTestResponse{body: `{"country":"JP"}`}, ipSB: publicIPCountryTestResponse{body: `{"country_code":"JP"}`}, preferMirrors: false},
		{name: "conflicting countries", countryIS: publicIPCountryTestResponse{body: `{"country":"JP"}`}, ipSB: publicIPCountryTestResponse{body: `{"country_code":"US"}`}, preferMirrors: true},
		{name: "one lookup fails", countryIS: publicIPCountryTestResponse{body: `{"country":"JP"}`}, ipSB: publicIPCountryTestResponse{err: errors.New("unavailable")}, preferMirrors: true},
		{name: "one lookup reports China", countryIS: publicIPCountryTestResponse{body: `{"country":"US"}`}, ipSB: publicIPCountryTestResponse{body: `{"country_code":"CN"}`}, preferMirrors: true},
		{name: "invalid response", countryIS: publicIPCountryTestResponse{body: `{"country":"unknown"}`}, ipSB: publicIPCountryTestResponse{body: `{}`}, preferMirrors: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string]publicIPCountryTestResponse{
				"https://api.country.is/": test.countryIS,
				"https://api.ip.sb/geoip": test.ipSB,
			}
			client := &http.Client{Transport: agentRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				result, ok := responses[request.URL.String()]
				if !ok {
					return nil, errors.New("unexpected public IP country URL")
				}
				if result.err != nil {
					return nil, result.err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Body:       io.NopCloser(strings.NewReader(result.body)),
					Header:     make(http.Header),
					Request:    request,
				}, nil
			})}
			if got := shouldPreferOfficialPackageMirrors(context.Background(), client); got != test.preferMirrors {
				t.Fatalf("prefer mirrors = %t, want %t", got, test.preferMirrors)
			}
		})
	}
}

func TestOfficialPackageDownloadSourcesRespectRegionPreference(t *testing.T) {
	resolved := OfficialPackage{
		URL:          "https://github.com/official.zip",
		FallbackURLs: []string{"https://sm.alliedmods.net/official.zip"},
		MirrorURLs:   []string{"https://gh-proxy.com/github.zip", "https://ghfast.top/github.zip"},
	}
	officialFirst := []string{
		resolved.URL,
		resolved.FallbackURLs[0],
		resolved.MirrorURLs[0],
		resolved.MirrorURLs[1],
	}
	if got := officialPackageDownloadSources(resolved); !reflect.DeepEqual(got, officialFirst) {
		t.Fatalf("official-first sources = %#v", got)
	}

	resolved.PreferMirrors = true
	mirrorFirst := []string{
		resolved.MirrorURLs[0],
		resolved.MirrorURLs[1],
		resolved.URL,
		resolved.FallbackURLs[0],
	}
	if got := officialPackageDownloadSources(resolved); !reflect.DeepEqual(got, mirrorFirst) {
		t.Fatalf("mirror-first sources = %#v", got)
	}
}
