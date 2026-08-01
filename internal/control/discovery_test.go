package control

import "testing"

func TestNormalizeDiscoveryRequestBounds(t *testing.T) {
	request, err := normalizeDiscoveryRequest(discoveryRequest{})
	if err != nil || request.MaxDepth != 6 || request.MaxEntries != 5000 {
		t.Fatalf("defaults = %#v, err = %v", request, err)
	}
	for _, request := range []discoveryRequest{{MaxDepth: 9}, {MaxEntries: 20001}, {MaxDepth: -1}, {MaxEntries: -1}} {
		if _, err := normalizeDiscoveryRequest(request); err == nil {
			t.Fatalf("expected %#v to be rejected", request)
		}
	}
}

func TestExternalInstanceJobAllowlist(t *testing.T) {
	allowed := []struct {
		action  string
		payload map[string]any
	}{
		{"restart", nil}, {"a2s.info", nil}, {"list", nil}, {"backup", nil},
		{"file", map[string]any{"operation": "read"}}, {"delete", map[string]any{"mode": "detach"}},
	}
	for _, test := range allowed {
		if !externalInstanceJobAllowed(test.action, test.payload) {
			t.Errorf("expected %s %#v to be allowed", test.action, test.payload)
		}
	}
	denied := []struct {
		action  string
		payload map[string]any
	}{
		{"start", nil}, {"stop", nil}, {"update", nil}, {"configure", nil},
		{"file", map[string]any{"operation": "write"}}, {"delete", map[string]any{"mode": "quarantine"}},
	}
	for _, test := range denied {
		if externalInstanceJobAllowed(test.action, test.payload) {
			t.Errorf("expected %s %#v to be denied", test.action, test.payload)
		}
	}
}
