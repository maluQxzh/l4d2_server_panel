package control

import (
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestValidateManagedFileAPIPath(t *testing.T) {
	for _, value := range []string{"left4dead2/cfg/server.cfg", "addons/sourcemod/configs/admins.cfg", "logs/server.log"} {
		if got, err := validateManagedFileAPIPath(value); err != nil || got != value {
			t.Fatalf("validate %q = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"../secret", "/etc/passwd", `C:\\Windows\\win.ini`, "left4dead2/bin/server.dll", "left4dead2/cfg/l4d2panel.cfg"} {
		if _, err := validateManagedFileAPIPath(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestValidateManagedFileAPIMutationPathRejectsPackages(t *testing.T) {
	for _, value := range []string{
		"left4dead2/addons/evil.vpk",
		"left4dead2/addons/sourcemod/plugins/evil.smx",
		"left4dead2/addons/sourcemod/extensions/evil.dll",
		"left4dead2/addons/sourcemod/extensions/evil.so",
		"left4dead2/addons/metamod.vdf",
	} {
		if _, err := validateManagedFileAPIMutationPath(value); err == nil {
			t.Fatalf("mutation path %q was accepted", value)
		}
	}
	if got, err := validateManagedFileAPIMutationPath("left4dead2/cfg/server.cfg"); err != nil || got != "left4dead2/cfg/server.cfg" {
		t.Fatalf("safe config path = %q, %v", got, err)
	}
}

func TestCanonicalLifecyclePayloadRejectsProcessOverrides(t *testing.T) {
	instance := domain.Instance{ID: "instance", NodeID: "node"}
	for _, field := range []string{"executable", "args", "env", "instance", "port", "rcon_password"} {
		if _, err := canonicalLifecyclePayload("start", map[string]any{field: "malicious"}, instance); err == nil {
			t.Fatalf("start payload field %q was accepted", field)
		}
	}
	payload, err := canonicalLifecyclePayload("start", map[string]any{"health_timeout_seconds": 30}, instance)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := payload["instance"].(domain.Instance)
	if !ok || got.ID != instance.ID || got.NodeID != instance.NodeID {
		t.Fatalf("canonical start payload = %#v", payload)
	}
}

func TestValidateStructuredConfigRejectsSecretCVars(t *testing.T) {
	for _, name := range []string{"rcon_password", "SV_PASSWORD"} {
		if err := validateStructuredConfig(domain.InstanceConfig{CVars: map[string]string{name: "plaintext"}}); err == nil {
			t.Fatalf("secret CVar %q was accepted", name)
		}
	}
}
