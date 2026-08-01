package agent

import "testing"

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
