package control

import "testing"

func TestValidInstancePorts(t *testing.T) {
	tests := []struct {
		name      string
		port      int
		tvPort    int
		wantValid bool
	}{
		{name: "automatic game port", port: 0, tvPort: 27020, wantValid: true},
		{name: "disabled SourceTV", port: 27015, tvPort: 0, wantValid: true},
		{name: "distinct ports", port: 27015, tvPort: 27020, wantValid: true},
		{name: "same port", port: 27015, tvPort: 27015, wantValid: false},
		{name: "negative game port", port: -1, tvPort: 0, wantValid: false},
		{name: "game port too large", port: 65536, tvPort: 0, wantValid: false},
		{name: "SourceTV port too large", port: 27015, tvPort: 65536, wantValid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validInstancePorts(test.port, test.tvPort); got != test.wantValid {
				t.Fatalf("validInstancePorts(%d, %d) = %v, want %v", test.port, test.tvPort, got, test.wantValid)
			}
		})
	}
}
