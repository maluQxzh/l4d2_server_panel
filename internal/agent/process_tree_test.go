package agent

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	processinfo "github.com/shirou/gopsutil/v4/process"
)

const processTreeHelperMarker = "--l4d2panel-process-tree-helper"

// TestProcessTreeHelper is re-executed by TestProcessManagerStopsEntireTree.
// Keeping the helper in the test binary lets the same test exercise Windows
// Job Objects and Unix process groups without relying on shell commands.
func TestProcessTreeHelper(t *testing.T) {
	marker := -1
	for index, argument := range os.Args {
		if argument == processTreeHelperMarker {
			marker = index
			break
		}
	}
	if marker < 0 {
		return
	}
	if len(os.Args) <= marker+2 {
		os.Exit(91)
	}
	mode, pidFile := os.Args[marker+1], os.Args[marker+2]
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$", "--", processTreeHelperMarker, "child", pidFile)
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(93)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestProcessManagerStopsEntireTree(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	pidFile := filepath.Join(dataDir, "child.pid")
	manager, err := NewProcessManager(dataDir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Start(ProcessSpec{
		InstanceID: "tree-test",
		Dir:        dataDir,
		Executable: os.Args[0],
		Args:       []string{"-test.run=^TestProcessTreeHelper$", "--", processTreeHelperMarker, "parent", pidFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop("tree-test", 250*time.Millisecond) })

	childPID, err := waitForHelperPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop("tree-test", 750*time.Millisecond); err != nil {
		t.Fatalf("stop managed process tree: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		exists, lookupErr := processinfo.PidExists(int32(childPID))
		if lookupErr == nil && !exists {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child process %d survived managed tree termination", childPID)
}

func waitForHelperPID(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return pid, nil
			}
			lastErr = parseErr
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0, fmt.Errorf("helper child did not publish a PID: %w", lastErr)
}
