//go:build windows

package agent

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"
)

var procGetNumberOfConsoleInputEvents = syscall.NewLazyDLL("kernel32.dll").NewProc("GetNumberOfConsoleInputEvents")

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
	if err := cmd.Wait(); err != nil {
		t.Fatalf("console-input helper failed: %v\n%s", err, output.String())
	}
}
