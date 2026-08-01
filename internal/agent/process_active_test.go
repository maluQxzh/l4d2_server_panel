package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestProcessManagerHasActiveProcessUsesOwnershipNotHealth(t *testing.T) {
	manager := &ProcessManager{items: map[string]*managedProcess{
		"degraded-marker": {state: ProcessState{Status: "degraded"}},
		"launching":       {state: ProcessState{Status: "starting"}, launching: true},
		"live-degraded":   {state: ProcessState{Status: "degraded"}, cmd: &exec.Cmd{}},
	}}

	if manager.HasActiveProcess("missing") || manager.HasActiveProcess("degraded-marker") {
		t.Fatal("missing or stopped degraded process was reported active")
	}
	if !manager.HasActiveProcess("launching") || !manager.HasActiveProcess("live-degraded") {
		t.Fatal("launching or owned live process was reported inactive")
	}
}

func TestRestoreRejectsLiveDegradedProcess(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	manager, err := NewProcessManager(dataDir, executor.logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.items["instance"] = &managedProcess{
		state: ProcessState{Status: "degraded"},
		cmd:   &exec.Cmd{},
	}
	manager.mu.Unlock()
	executor.processes = manager

	_, err = executor.restore(context.Background(), domain.Job{
		ID: uuid.NewString(), InstanceID: "instance", Type: "restore",
	}, func(int, string) {})
	if err == nil || !strings.Contains(err.Error(), "instance must be stopped before restore") {
		t.Fatalf("live degraded restore error = %v", err)
	}
}
