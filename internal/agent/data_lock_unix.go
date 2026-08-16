//go:build !windows

package agent

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type agentDataLock interface {
	io.Closer
}

type unixAgentDataLock struct{ file *os.File }

func (l *unixAgentDataLock) Close() error {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

func acquireAgentDataLock(path string) (agentDataLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open Agent data_dir lock")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another Agent process is already using this data_dir: %w", err)
	}
	return &unixAgentDataLock{file: file}, nil
}
