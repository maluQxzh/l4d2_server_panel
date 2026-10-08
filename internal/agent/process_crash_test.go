package agent

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticRestartRetriesTransientLaunchFailure(t *testing.T) {
	base := t.TempDir()
	m, err := NewProcessManager(base, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "hold")
	if err := os.WriteFile(marker, []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate a crashed process whose working directory is temporarily absent.
	// The automatic launch must retry once the directory is available again.
	dir := filepath.Join(base, "temporarily-unavailable")
	p := &managedProcess{spec: ProcessSpec{InstanceID: "transient", Dir: dir, Executable: os.Args[0], AutoRestart: true,
		Args: []string{"-test.run=^TestCrashRestartHelper$", "--", "--crash-restart-helper", marker}},
		state: ProcessState{Status: "stopped"}, crashes: []time.Time{time.Now().UTC()}}
	m.items[p.spec.InstanceID] = p
	t.Cleanup(func() { _ = m.Stop(p.spec.InstanceID, 100*time.Millisecond) })
	m.restartAfterCrash(p)
	if got := m.State(p.spec.InstanceID); got.Status != "stopped" || got.ExitError == "" {
		t.Fatalf("one failed launch prematurely stopped recovery: %+v", got)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		if got := m.State(p.spec.InstanceID); got.Status == "running" && got.PID > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("transient failure was not retried: %+v", m.State(p.spec.InstanceID))
}

func TestAutomaticRestartLaunchFailuresShareCrashBudget(t *testing.T) {
	base := t.TempDir()
	var output strings.Builder
	m, err := NewProcessManager(base, log.New(&output, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &managedProcess{spec: ProcessSpec{InstanceID: "failure-budget", Dir: base, Executable: filepath.Join(base, "missing"), AutoRestart: true},
		state: ProcessState{Status: "stopped"}, crashes: []time.Time{time.Now().UTC()}}
	m.items[p.spec.InstanceID] = p
	t.Cleanup(func() { m.RequestStop(p.spec.InstanceID) })
	for i := 0; i < 5; i++ {
		m.restartAfterCrash(p)
	}
	if got := m.State(p.spec.InstanceID); got.Status != "crash_loop" || got.ExitError == "" {
		t.Fatalf("retry budget not enforced: %+v", got)
	}
	if !strings.Contains(output.String(), "automatic restart limit reached") {
		t.Fatal("retry limit has no diagnostic")
	}
	before := p.done
	m.restartAfterCrash(p)
	if p.done != before {
		t.Fatal("stale timer bypassed crash-loop protection")
	}
}

func TestAutomaticRestartDiscardsExpiredFailures(t *testing.T) {
	base := t.TempDir()
	m, err := NewProcessManager(base, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &managedProcess{spec: ProcessSpec{InstanceID: "expired", Dir: base, Executable: filepath.Join(base, "missing"), AutoRestart: true}, state: ProcessState{Status: "stopped"}}
	for range 5 {
		p.crashes = append(p.crashes, time.Now().Add(-11*time.Minute))
	}
	m.items[p.spec.InstanceID] = p
	t.Cleanup(func() { m.RequestStop(p.spec.InstanceID) })
	m.restartAfterCrash(p)
	if got := m.State(p.spec.InstanceID); got.Status == "crash_loop" {
		t.Fatalf("expired failures exhausted the retry budget: %+v", got)
	}
	if len(p.crashes) != 1 {
		t.Fatalf("expired failures retained: %v", p.crashes)
	}
}

func TestAutomaticRestartFailureCanBeCanceled(t *testing.T) {
	for _, action := range []string{"stop", "disable", "replaced", "launching"} {
		t.Run(action, func(t *testing.T) {
			base := t.TempDir()
			m, err := NewProcessManager(base, log.New(io.Discard, "", 0), nil)
			if err != nil {
				t.Fatal(err)
			}
			p := &managedProcess{spec: ProcessSpec{InstanceID: action, Dir: base, Executable: filepath.Join(base, "missing"), AutoRestart: true}, state: ProcessState{Status: "stopped"}}
			m.items[action] = p
			m.restartAfterCrash(p)
			t.Cleanup(func() { m.mu.Lock(); p.stopRequested = true; m.mu.Unlock() })
			before := p.done
			switch action {
			case "stop":
				m.RequestStop(action)
			case "disable":
				m.SetAutoRestart(action, false)
			case "replaced":
				m.items[action] = &managedProcess{}
			case "launching":
				p.launching = true
			}
			m.restartAfterCrash(p)
			if p.done != before {
				t.Fatal("obsolete retry attempted another launch")
			}
		})
	}
}

func TestCrashRestartHelper(t *testing.T) {
	for index, arg := range os.Args {
		if arg != "--crash-restart-helper" || index+1 >= len(os.Args) {
			continue
		}
		marker := os.Args[index+1]
		if marker == "child" {
			for {
				time.Sleep(time.Hour)
			}
		}
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			child := exec.Command(os.Args[0], "-test.run=^TestCrashRestartHelper$", "--", "--crash-restart-helper", "child")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				os.Exit(92)
			}
			if err := os.WriteFile(marker, []byte("crashed"), 0600); err != nil {
				os.Exit(93)
			}
			os.Exit(7)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
}

func TestProcessManagerRestartsAfterCrashWithInheritedOutput(t *testing.T) {
	dir := t.TempDir()
	m, err := NewProcessManager(dir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Start(ProcessSpec{InstanceID: "crash", Dir: dir, Executable: os.Args[0],
		Args: []string{"-test.run=^TestCrashRestartHelper$", "--", "--crash-restart-helper", filepath.Join(dir, "once")}, AutoRestart: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop("crash", 100*time.Millisecond) })
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		state := m.State("crash")
		if state.Status == "running" && state.PID != first.PID && state.PID > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("crashed process was not restarted: first=%+v current=%+v", first, m.State("crash"))
}

func TestProcessManagerCrashLoopStopsAutomaticRestarts(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "once")
	if err := os.WriteFile(marker, []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewProcessManager(dir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Start(ProcessSpec{InstanceID: "loop", Dir: dir, Executable: os.Args[0],
		Args: []string{"-test.run=^TestCrashRestartHelper$", "--", "--crash-restart-helper", marker}, AutoRestart: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop("loop", 100*time.Millisecond) })
	m.mu.Lock()
	p := m.items["loop"]
	for range 5 {
		p.crashes = append(p.crashes, time.Now())
	}
	err = p.cmd.Process.Kill()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.State("loop").Status == "crash_loop" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("crash loop was not limited: %+v", m.State("loop"))
}
