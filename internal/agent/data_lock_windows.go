//go:build windows

package agent

import (
	"fmt"
	"io"

	"golang.org/x/sys/windows"
)

type agentDataLock interface {
	io.Closer
}

type windowsAgentDataLock struct{ handle windows.Handle }

func (l *windowsAgentDataLock) Close() error { return windows.CloseHandle(l.handle) }

func acquireAgentDataLock(path string) (agentDataLock, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("another Agent process is already using this data_dir: %w", err)
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("Agent data_dir lock must not be a reparse point")
	}
	return &windowsAgentDataLock{handle: handle}, nil
}
