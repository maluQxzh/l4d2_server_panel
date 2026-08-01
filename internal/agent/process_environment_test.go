package agent

import (
	"strings"
	"testing"
)

func TestSanitizedProcessEnvironmentDropsAgentSecretsAndLoaderOverrides(t *testing.T) {
	t.Setenv("L4D2PANEL_AGENT_CREDENTIAL", "secret")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("LD_LIBRARY_PATH", "/tmp/evil")
	instanceDir := t.TempDir()
	joined := strings.Join(sanitizedProcessEnvironment(instanceDir), "\n")
	for _, forbidden := range []string{"L4D2PANEL_AGENT_CREDENTIAL", "LD_PRELOAD", "LD_LIBRARY_PATH", "secret", "/tmp/evil"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sanitized environment leaked %q: %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "HOME="+instanceDir) {
		t.Fatalf("sanitized environment does not isolate HOME: %s", joined)
	}
}
