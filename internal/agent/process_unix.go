//go:build !windows && !linux

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

type platformProcess struct{ pid int }

func platformProcessOutputFailure(string) string { return "" }

func preparePlatformCommand(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func attachPlatformProcess(cmd *exec.Cmd) (*platformProcess, error) {
	return &platformProcess{pid: cmd.Process.Pid}, nil
}
func gracefulPlatformProcess(p *platformProcess, process *os.Process) error {
	if p != nil && p.pid > 0 {
		return syscall.Kill(-p.pid, syscall.SIGTERM)
	}
	return process.Signal(syscall.SIGTERM)
}
func forcePlatformProcess(p *platformProcess, process *os.Process) error {
	if p != nil && p.pid > 0 {
		return syscall.Kill(-p.pid, syscall.SIGKILL)
	}
	return process.Kill()
}
func closePlatformProcess(_ *platformProcess) {}
