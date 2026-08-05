//go:build windows

package agent

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func mapFilesystemID(path string) (string, error) {
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	if err := validateWindowsHandle(handle, true); err != nil {
		return "", err
	}
	var serial uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, &serial, nil, nil, nil, 0); err != nil {
		return "", fmt.Errorf("read volume identity: %w", err)
	}
	if serial == 0 {
		return "", os.ErrInvalid
	}
	return fmt.Sprintf("windows:%08x", serial), nil
}
