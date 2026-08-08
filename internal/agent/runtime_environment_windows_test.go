package agent

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestValidateManagedSteamCMDPathRetriesWindowsBootstrapExitSeven(t *testing.T) {
	exitSeven := exec.Command("cmd.exe", "/c", "exit", "/b", "7").Run()
	var exitErr *exec.ExitError
	if !errors.As(exitSeven, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("create exit-code-7 error: %v", exitSeven)
	}

	calls := 0
	version, err := validateManagedSteamCMDPathWith(context.Background(), `C:\steamcmd\steamcmd.exe`, nil, func(context.Context, string, []string) (string, error) {
		calls++
		if calls == 1 {
			return "", exitSeven
		}
		return "available", nil
	})
	if err != nil || version != "available" {
		t.Fatalf("bootstrap retry result = %q, %v", version, err)
	}
	if calls != 2 {
		t.Fatalf("validator calls = %d, want 2", calls)
	}
}

func TestValidateManagedSteamCMDPathDoesNotRetryOtherExitCodes(t *testing.T) {
	exitTwo := exec.Command("cmd.exe", "/c", "exit", "/b", "2").Run()
	calls := 0
	_, err := validateManagedSteamCMDPathWith(context.Background(), `C:\steamcmd\steamcmd.exe`, nil, func(context.Context, string, []string) (string, error) {
		calls++
		return "", exitTwo
	})
	if err == nil {
		t.Fatal("non-bootstrap failure was accepted")
	}
	if calls != 1 {
		t.Fatalf("validator calls = %d, want 1", calls)
	}
}
