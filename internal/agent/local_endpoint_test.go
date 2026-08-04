package agent

import (
	"context"
	"net"
	"slices"
	"testing"
)

func TestLocalGameEndpointHostCannotBeRedirectedOffNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		candidates []string
		want       string
	}{
		{[]string{"127.0.0.2"}, "127.0.0.1"},
		{[]string{"[::1]"}, "::1"},
		{[]string{"0.0.0.0", "169.254.169.254"}, "127.0.0.1"},
		{[]string{"example.com", "::1"}, "::1"},
		{[]string{"10.0.0.5"}, "127.0.0.1"},
	}
	for _, test := range tests {
		if got := localGameEndpointHost(test.candidates...); got != test.want {
			t.Fatalf("localGameEndpointHost(%q) = %q, want %q", test.candidates, got, test.want)
		}
	}
}

func TestLocalGameEndpointHostsUseOnlyAssignedAndLoopbackAddresses(t *testing.T) {
	t.Parallel()
	got := localGameEndpointHostsWithAssigned(
		[]string{"10.0.0.8", "2001:db8::8", "10.0.0.8", "169.254.10.20"},
		"203.0.113.9",
		"10.0.0.8",
		"0.0.0.0",
	)
	want := []string{"10.0.0.8", "2001:db8::8", "127.0.0.1", "::1"}
	if !slices.Equal(got, want) {
		t.Fatalf("local hosts = %q, want %q", got, want)
	}
}

func TestLocalGameEndpointHostsPreserveSafeExplicitLoopback(t *testing.T) {
	t.Parallel()
	got := localGameEndpointHostsWithAssigned([]string{"192.168.1.20"}, "127.0.0.2")
	want := []string{"127.0.0.2", "192.168.1.20", "127.0.0.1", "::1"}
	if !slices.Equal(got, want) {
		t.Fatalf("local hosts = %q, want %q", got, want)
	}
}

func TestLocalGameEndpointHostsIncludeHostnameResolvedLoopback(t *testing.T) {
	t.Parallel()
	resolved := resolveLocalHostnameHosts(context.Background(), "maluQxzh", func(_ context.Context, hostname string) ([]net.IPAddr, error) {
		if hostname != "maluQxzh" {
			t.Fatalf("hostname = %q", hostname)
		}
		return []net.IPAddr{
			{IP: net.ParseIP("127.0.1.1")},
			{IP: net.ParseIP("192.168.1.20")},
			{IP: net.ParseIP("127.0.1.1")},
			{IP: net.ParseIP("203.0.113.9")},
			{IP: net.ParseIP("169.254.10.20")},
		}, nil
	})
	got := localGameEndpointHostsWithAssigned([]string{"192.168.1.20"}, resolved...)
	want := []string{"127.0.1.1", "192.168.1.20", "127.0.0.1", "::1"}
	if !slices.Equal(got, want) {
		t.Fatalf("local hosts = %q, want %q", got, want)
	}
}
