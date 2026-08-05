//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type managedDownloadFile struct {
	mu       sync.Mutex
	parent   int
	tmpName  string
	target   string
	file     *os.File
	options  secureWriteOptions
	finished bool
}

func createManagedDownloadFile(root, relative string, options secureWriteOptions) (*managedDownloadFile, error) {
	parent, leaf, err := openManagedParentLinux(root, relative, true)
	if err != nil {
		return nil, err
	}
	if options.RejectExistingLink {
		if err := validateManagedWriteTargetLinux(parent, leaf); err != nil {
			_ = unix.Close(parent)
			return nil, err
		}
	}
	fd := -1
	tmpName := ""
	for attempt := 0; attempt < 8; attempt++ {
		tmpName, err = managedTempName()
		if err != nil {
			_ = unix.Close(parent)
			return nil, fmt.Errorf("create managed download temporary name: %w", err)
		}
		fd, err = openatManagedLinux(parent, tmpName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(options.Mode.Perm()))
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		break
	}
	if err != nil {
		_ = unix.Close(parent)
		return nil, fmt.Errorf("create managed download temporary file: %w", err)
	}
	if fd < 0 {
		_ = unix.Close(parent)
		return nil, errors.New("could not allocate a managed download temporary file")
	}
	file := os.NewFile(uintptr(fd), tmpName)
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(parent, tmpName, 0)
		_ = unix.Close(parent)
		return nil, errors.New("create managed download file handle")
	}
	if err := file.Chmod(options.Mode.Perm()); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(parent, tmpName, 0)
		_ = unix.Close(parent)
		return nil, fmt.Errorf("set managed download file permissions: %w", err)
	}
	return &managedDownloadFile{parent: parent, tmpName: tmpName, target: leaf, file: file, options: options}, nil
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
	var err error
	if file.options.Replace {
		err = unix.Renameat(file.parent, file.tmpName, file.parent, file.target)
	} else {
		err = unix.Renameat2(file.parent, file.tmpName, file.parent, file.target, unix.RENAME_NOREPLACE)
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			err = unix.Linkat(file.parent, file.tmpName, file.parent, file.target, 0)
			if err == nil {
				_ = unix.Unlinkat(file.parent, file.tmpName, 0)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("commit managed download file: %w", err)
	}
	file.finished = true
	_ = file.file.Close()
	_ = unix.Fsync(file.parent)
	_ = unix.Close(file.parent)
	return nil
}

func (file *managedDownloadFile) Abort() {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.finished {
		return
	}
	file.finished = true
	_ = file.file.Close()
	_ = unix.Unlinkat(file.parent, file.tmpName, 0)
	_ = unix.Fsync(file.parent)
	_ = unix.Close(file.parent)
}
