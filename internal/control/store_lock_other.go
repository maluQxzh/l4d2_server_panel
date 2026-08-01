//go:build !windows && !linux

package control

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type storeLock interface {
	io.Closer
}

func secureSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure SQLite file %s: %w", candidate, err)
		}
	}
	return nil
}

type unixStoreLock struct{ file *os.File }

func (l *unixStoreLock) Close() error {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

func validateLocalDatabasePath(string) error { return nil }

func acquireStoreLock(path string) (storeLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another Control process may already be using this database: %w", err)
	}
	return &unixStoreLock{file: file}, nil
}
