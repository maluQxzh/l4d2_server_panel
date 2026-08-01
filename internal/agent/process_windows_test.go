//go:build windows

package agent

import (
	"os"
	"os/exec"
	"testing"
)

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
}
