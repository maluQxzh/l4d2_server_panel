//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

type managedDownloadFile struct {
	mu       sync.Mutex
	parent   windows.Handle
	handle   windows.Handle
	target   string
	file     *os.File
	options  secureWriteOptions
	finished bool
}

func createManagedDownloadFile(root, relative string, options secureWriteOptions) (*managedDownloadFile, error) {
	parent, leaf, err := openManagedParentWindows(root, relative, true)
	if err != nil {
		return nil, err
	}
	if options.RejectExistingLink {
		if err := validateManagedWriteTargetWindows(parent, leaf); err != nil {
			_ = windows.CloseHandle(parent)
			return nil, err
		}
	}
	var handle windows.Handle
	for attempt := 0; attempt < 8; attempt++ {
		tmpName, nameErr := managedTempName()
		if nameErr != nil {
			_ = windows.CloseHandle(parent)
			return nil, fmt.Errorf("create managed download temporary name: %w", nameErr)
		}
		handle, err = openRelativeWindows(parent, tmpName, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
			managedWindowsWriteShare, windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_ATTRIBUTE_NORMAL)
		if err == windows.STATUS_OBJECT_NAME_COLLISION {
			continue
		}
		break
	}
	if err != nil {
		_ = windows.CloseHandle(parent)
		return nil, fmt.Errorf("create managed download temporary file: %w", err)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		_ = windows.CloseHandle(parent)
		return nil, errors.New("could not allocate a managed download temporary file")
	}
	file := os.NewFile(uintptr(handle), "managed-download")
	if file == nil {
		_ = deleteWindowsHandle(handle)
		_ = windows.CloseHandle(handle)
		_ = windows.CloseHandle(parent)
		return nil, errors.New("create managed download file handle")
	}
	_ = file.Chmod(options.Mode.Perm())
	return &managedDownloadFile{parent: parent, handle: handle, target: leaf, file: file, options: options}, nil
}

func (file *managedDownloadFile) WriteAt(raw []byte, offset int64) (int, error) {
	return file.file.WriteAt(raw, offset)
}

func (file *managedDownloadFile) ReadAt(raw []byte, offset int64) (int, error) {
	return file.file.ReadAt(raw, offset)
}

func (file *managedDownloadFile) Truncate(size int64) error { return file.file.Truncate(size) }
func (file *managedDownloadFile) Sync() error               { return file.file.Sync() }

func (file *managedDownloadFile) Size() (int64, error) {
	info, err := file.file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (file *managedDownloadFile) Commit() error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.finished {
		return errors.New("managed download file is already finalized")
	}
	if err := file.file.Sync(); err != nil {
		return fmt.Errorf("sync managed download file: %w", err)
	}
	if err := renameWindowsHandle(file.handle, file.parent, file.target, file.options.Replace); err != nil {
		return fmt.Errorf("commit managed download file: %w", err)
	}
	file.finished = true
	_ = file.file.Close()
	_ = windows.CloseHandle(file.parent)
	return nil
}

func (file *managedDownloadFile) Abort() {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.finished {
		return
	}
	file.finished = true
	_ = deleteWindowsHandle(file.handle)
	_ = file.file.Close()
	_ = windows.CloseHandle(file.parent)
}
