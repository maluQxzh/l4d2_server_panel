//go:build linux

package control

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type storeLock interface {
	io.Closer
}

type unixStoreLock struct{ file *os.File }

func (l *unixStoreLock) Close() error {
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

func validateLocalDatabasePath(path string) error {
	var stat unix.Statfs_t
	probe := filepath.Dir(path)
	for {
		if err := unix.Statfs(probe, &stat); err == nil {
			break
		} else if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("inspect database filesystem: %w", err)
		}
		next := filepath.Dir(probe)
		if next == probe {
			return errors.New("cannot resolve database filesystem")
		}
		probe = next
	}
	switch uint64(stat.Type) {
	case 0x6969, 0xFF534D42, 0xFE534D42, 0x517B:
		return errors.New("SQLite database must not be stored on NFS, SMB, or CIFS")
	}
	return nil
}

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

func secureSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure SQLite file %s: %w", candidate, err)
		}
	}
	return nil
}
