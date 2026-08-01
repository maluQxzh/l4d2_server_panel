//go:build windows

package control

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type storeLock interface {
	io.Closer
}

type windowsStoreLock struct{ handle windows.Handle }

func (l *windowsStoreLock) Close() error { return windows.CloseHandle(l.handle) }

func validateLocalDatabasePath(path string) error {
	if strings.HasPrefix(path, `\\`) {
		return errors.New("SQLite database must not use a UNC path")
	}
	volume := filepath.VolumeName(path)
	if volume == "" {
		return errors.New("SQLite database path has no local volume")
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return err
	}
	if windows.GetDriveType(root) == windows.DRIVE_REMOTE {
		return errors.New("SQLite database must be stored on a local drive")
	}
	return nil
}

func acquireStoreLock(path string) (storeLock, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("another Control process may already be using this database: %w", err)
	}
	return &windowsStoreLock{handle: handle}, nil
}

func secureSQLiteFiles(string) error { return nil }
