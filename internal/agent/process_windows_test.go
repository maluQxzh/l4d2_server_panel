//go:build windows

package agent

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestNativeExceptionRestartHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--native-exception-helper" || i+1 >= len(os.Args) {
			continue
		}
		// Refuse to raise an exception if the Job protection regresses; the test
		// must never leave an interactive Windows crash dialog on the desktop.
		mode, _, _ := kernel32.NewProc("GetErrorMode").Call()
		if mode&2 == 0 {
			os.Exit(90)
		}
		marker := os.Args[i+1]
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			if err := os.WriteFile(marker, []byte("native crash"), 0600); err != nil {
				os.Exit(91)
			}
			kernel32.NewProc("RaiseException").Call(0xe0123456, 1, 0, 0)
			os.Exit(92)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
}

func TestWindowsNativeExceptionAutomaticallyRestarts(t *testing.T) {
	dir := t.TempDir()
	m, err := NewProcessManager(dir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "native-crash")
	first, err := m.Start(ProcessSpec{InstanceID: "native-crash", Dir: dir, Executable: os.Args[0], AutoRestart: true,
		Args: []string{"-test.run=^TestNativeExceptionRestartHelper$", "--", "--native-exception-helper", marker}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop("native-crash", 100*time.Millisecond) })
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		state := m.State("native-crash")
		if state.Status == "running" && state.PID > 0 && state.PID != first.PID {
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("helper did not raise native exception")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("native crash was not restarted: %+v", m.State("native-crash"))
}

var procGetNumberOfConsoleInputEvents = syscall.NewLazyDLL("kernel32.dll").NewProc("GetNumberOfConsoleInputEvents")

func TestPlatformProcessOutputFailureRecognizesSRCDSConsoleError(t *testing.T) {
	got := platformProcessOutputFailure("CTextConsoleWin32::GetLine: !GetNumberOfConsoleInputEvents")
	if got == "" {
		t.Fatal("SRCDS console-input failure was not recognized")
	}
	if got := platformProcessOutputFailure("Console initialized."); got != "" {
		t.Fatalf("ordinary SRCDS output was classified as a failure: %q", got)
	}
}

func TestProcessManagerMarksSRCDSConsoleFailureDegraded(t *testing.T) {
	const instanceID = "console-failure"
	process := &managedProcess{
		spec:  ProcessSpec{InstanceID: instanceID},
		cmd:   exec.Command(os.Args[0]),
		state: ProcessState{Status: "running", PID: 42},
	}
	manager := &ProcessManager{items: map[string]*managedProcess{instanceID: process}}
	manager.recordOutputFailure(process, "CTextConsoleWin32::GetLine: !GetNumberOfConsoleInputEvents")
	state := manager.State(instanceID)
	if state.Status != "degraded" || state.ExitError == "" {
		t.Fatalf("console failure state = %#v, want degraded with an explicit error", state)
	}
}

func TestPreparePlatformCommandStartsSuspended(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	preparePlatformCommand(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("preparePlatformCommand did not install SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&createSuspended == 0 {
		t.Fatal("managed process is not created suspended")
	}
	if cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatal("managed process lost CREATE_NEW_PROCESS_GROUP")
	}
	if cmd.SysProcAttr.CreationFlags&createNewConsole == 0 {
		t.Fatal("managed process does not receive a console input buffer")
	}
	stdin, ok := cmd.Stdin.(*os.File)
	if !ok || stdin.Fd() != 0 {
		t.Fatalf("managed process stdin = %#v, want NULL handle for automatic console initialization", cmd.Stdin)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("managed process console window is not hidden")
	}
}

func TestPreparePlatformCommandProvidesConsoleInput(t *testing.T) {
	const helperEnvironment = "L4D2P_TEST_CONSOLE_INPUT=1"
	if os.Getenv("L4D2P_TEST_CONSOLE_INPUT") == "1" {
		handle, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
		if err != nil {
			t.Fatalf("GetStdHandle(STD_INPUT_HANDLE): %v", err)
		}
		var events uint32
		ok, _, callErr := procGetNumberOfConsoleInputEvents.Call(uintptr(handle), uintptr(unsafe.Pointer(&events)))
		if ok == 0 {
			t.Fatalf("GetNumberOfConsoleInputEvents: %v", callErr)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPreparePlatformCommandProvidesConsoleInput$")
	cmd.Env = append(os.Environ(), helperEnvironment)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	preparePlatformCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	platform, err := attachPlatformProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("attach helper process: %v", err)
	}
	defer closePlatformProcess(platform)
	var limits jobObjectExtendedLimit
	ok, _, queryErr := kernel32.NewProc("QueryInformationJobObject").Call(uintptr(platform.job), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits), 0)
	if ok == 0 {
		t.Fatalf("query managed Job limits: %v", queryErr)
	}
	want := uint32(jobObjectLimitKillOnJobClose | jobObjectLimitDieOnUnhandledException)
	if limits.BasicLimitInformation.LimitFlags&want != want {
		t.Fatalf("managed Job limits = %#x, missing kill-on-close or native-exception termination", limits.BasicLimitInformation.LimitFlags)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("console-input helper failed: %v\n%s", err, output.String())
	}
}
