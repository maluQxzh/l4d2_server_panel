package agent

import (
	"io"
	"log"
	"path/filepath"
	"testing"
)

func TestRecoverInstancesMarksUnsafeRecoveryDegraded(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "instances")
	data := filepath.Join(base, "data")
	paths, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "server-1", RootIndex: 0, RelativePath: "server-1", Executable: "missing-server.exe", Args: []string{"-console", "-game", "left4dead2"}, AutoRestart: true, DesiredRunning: true, RCONPort: 39091}); err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	processes, err := NewProcessManager(data, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{logger: logger, paths: paths, registry: registry, processes: processes}
	a.recoverInstances()
	if state := processes.State("server-1"); state.Status != "degraded" {
		t.Fatalf("recovery state = %#v", state)
	}
	a.config = &Config{}
	heartbeat := a.heartbeat()
	if heartbeat.Instances["server-1"] != "degraded" {
		t.Fatalf("heartbeat = %#v", heartbeat.Instances)
	}
}

func TestRecoverInstancesHonorsExplicitStoppedIntent(t *testing.T) {
	base := t.TempDir()
	paths, err := NewPathGuard([]string{filepath.Join(base, "instances")})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "stopped", RootIndex: 0, RelativePath: "stopped", AutoRestart: true, DesiredRunning: false}); err != nil {
		t.Fatal(err)
	}
	processes, err := NewProcessManager(filepath.Join(base, "data"), log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{logger: log.New(io.Discard, "", 0), paths: paths, registry: registry, processes: processes}
	a.recoverInstances()
	if _, ok := processes.States()["stopped"]; ok {
		t.Fatal("explicitly stopped instance was recovered")
	}
}

func TestHeartbeatPreservesCrashLoopSignal(t *testing.T) {
	base := t.TempDir()
	registry, err := OpenRegistry(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "looping"}); err != nil {
		t.Fatal(err)
	}
	processes, err := NewProcessManager(filepath.Join(base, "data"), log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	processes.items["looping"] = &managedProcess{state: ProcessState{Status: "crash_loop"}}
	a := &Agent{config: &Config{}, registry: registry, processes: processes}
	if got := a.heartbeat().Instances["looping"]; got != "crash_loop" {
		t.Fatalf("heartbeat status = %q, want crash_loop", got)
	}
}
