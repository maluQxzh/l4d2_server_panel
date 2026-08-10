package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestTogglePackageRejectsDisableWhileProcessIsActive(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	executor.processes = &ProcessManager{items: map[string]*managedProcess{
		"instance": {state: ProcessState{Status: "running"}},
	}}
	packagePath := filepath.Join(instancePath, "left4dead2", "addons", "campaign.vpk")
	if err := os.MkdirAll(filepath.Dir(packagePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packagePath, []byte("package"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, err := executor.togglePackage(context.Background(), domain.Job{
		InstanceID: "instance",
		Payload: map[string]any{
			"enabled": false,
			"kind":    "vpk",
			"path":    "left4dead2/addons/campaign.vpk",
		},
	}, func(int, string) {})
	if err == nil || !strings.Contains(err.Error(), "must be stopped") {
		t.Fatalf("active process package disable error = %v", err)
	}
	if _, statErr := os.Stat(packagePath); statErr != nil {
		t.Fatalf("enabled package changed despite rejection: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(instancePath, "left4dead2", "addons", "panel-disabled", "campaign.vpk")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("disabled package unexpectedly exists: %v", statErr)
	}
}
