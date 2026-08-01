package agent

import (
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestBuildServerConfigRejectsSecretCVarOverrides(t *testing.T) {
	for _, name := range []string{"rcon_password", "SV_PASSWORD"} {
		_, err := buildServerConfig(domain.InstanceConfig{CVars: map[string]string{name: "plaintext"}}, "encrypted-secret")
		if err == nil {
			t.Fatalf("secret CVar %q was accepted", name)
		}
	}
}
