package containerupdate

import (
	"strings"
	"testing"
)

func TestRejectDowngrade(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		target    string
		wantError bool
		contains  string
	}{
		{name: "same release", current: "1.2.3", target: "v1.2.3"},
		{name: "newer patch", current: "v1.2.3", target: "1.2.4"},
		{name: "newer major", current: "1.2.3", target: "2.0.0"},
		{name: "older patch", current: "1.2.3", target: "1.2.2", wantError: true, contains: "downgrade"},
		{name: "development reinstall", current: "dev", target: "dev"},
		{name: "development mismatch", current: "dev", target: "1.2.3", wantError: true, contains: "same-version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := rejectDowngrade(test.current, test.target)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), test.contains)) {
				t.Fatalf("rejectDowngrade(%q, %q) error = %v", test.current, test.target, err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("rejectDowngrade(%q, %q) error = %v", test.current, test.target, err)
			}
		})
	}
}

func TestNormalizeArchitecture(t *testing.T) {
	for input, expected := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "amd64": "amd64"} {
		if actual := normalizeArchitecture(input); actual != expected {
			t.Fatalf("normalizeArchitecture(%q) = %q, want %q", input, actual, expected)
		}
	}
}
