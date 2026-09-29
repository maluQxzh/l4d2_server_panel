package agent

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
