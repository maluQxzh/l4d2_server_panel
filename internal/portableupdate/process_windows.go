//go:build windows

package portableupdate

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func startUpgradeWorker(worker, root, jobID string) error {
	command := exec.Command(worker, "portable", "upgrade-worker", "--root", root, "--job", jobID)
	command.Stdout, command.Stderr, command.Stdin = nil, nil, nil
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x08000000, HideWindow: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func replaceFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
