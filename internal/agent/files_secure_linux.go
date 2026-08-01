//go:build linux

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const managedLinuxResolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV

// openManagedFileNoFollow resolves each component relative to an already-open
// directory. No path lookup after the parent handle is acquired can redirect
// the operation through a symlink or mount outside the instance.
func openManagedFileNoFollow(instancePath, cleaned string) (*os.File, error) {
	parent, leaf, err := openManagedParentLinux(instancePath, cleaned, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)

	fd, err := openatManagedLinux(parent, leaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, managedLinuxPathError("open managed file", err)
	}
	file := os.NewFile(uintptr(fd), leaf)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create managed file handle")
	}
	return file, nil
}

func statManagedDirectoryNoFollow(instancePath, cleaned string) (os.FileInfo, error) {
	fd, err := openManagedDirectoryLinux(instancePath, cleaned)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), cleaned)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create managed directory handle")
	}
	defer file.Close()
	return file.Stat()
}

func listManagedDirectoryNoFollow(instancePath, cleaned string) ([]ManagedFile, error) {
	fd, err := openManagedDirectoryLinux(instancePath, cleaned)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), cleaned)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create managed directory handle")
	}
	defer directory.Close()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	result := make([]ManagedFile, 0, len(names))
	for _, name := range names {
		childFD, openErr := openatManagedLinux(fd, name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			continue
		}
		child := os.NewFile(uintptr(childFD), name)
		if child == nil {
			_ = unix.Close(childFD)
			continue
		}
		info, statErr := child.Stat()
		_ = child.Close()
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		result = append(result, ManagedFile{
			Name: name, Path: filepath.ToSlash(filepath.Join(cleaned, name)), IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime(),
		})
	}
	return result, nil
}

func openManagedDirectoryLinux(instancePath, cleaned string) (int, error) {
	parent, _, err := openManagedParentLinux(instancePath, filepath.ToSlash(filepath.Join(cleaned, "_managed_dir_anchor_")), false)
	return parent, err
}

func writeManagedFileNoFollow(instancePath, cleaned string, raw []byte) error {
	_, err := writeFileWithinNoFollow(instancePath, cleaned, bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0o640))
	return err
}

func writeManagedFileAtLinux(parent int, leaf string, raw []byte) error {
	_, err := writeFileAtLinux(parent, leaf, bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0o640))
	return err
}

func writeFileWithinNoFollow(root, relative string, source io.Reader, options secureWriteOptions) (int64, error) {
	parent, leaf, err := openManagedParentLinux(root, relative, true)
	if err != nil {
		return 0, err
	}
	defer unix.Close(parent)
	return writeFileAtLinux(parent, leaf, source, options)
}

func writeFileAtLinux(parent int, leaf string, source io.Reader, options secureWriteOptions) (int64, error) {
	if options.RejectExistingLink {
		if err := validateManagedWriteTargetLinux(parent, leaf); err != nil {
			return 0, err
		}
	}
	var (
		fd      = -1
		tmpName string
		err     error
	)
	for attempt := 0; attempt < 8; attempt++ {
		tmpName, err = managedTempName()
		if err != nil {
			return 0, fmt.Errorf("create managed temporary name: %w", err)
		}
		fd, err = openatManagedLinux(parent, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(options.Mode.Perm()))
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		break
	}
	if err != nil {
		return 0, fmt.Errorf("create managed temporary file: %w", err)
	}
	if fd < 0 {
		return 0, errors.New("could not allocate a managed temporary file")
	}

	committed := false
	file := os.NewFile(uintptr(fd), tmpName)
	if file == nil {
		_ = unix.Close(fd)
		return 0, errors.New("create managed temporary file handle")
	}
	defer func() {
		_ = file.Close()
		if !committed {
			_ = unix.Unlinkat(parent, tmpName, 0)
		}
	}()
	if err := file.Chmod(options.Mode.Perm()); err != nil {
		return 0, fmt.Errorf("set managed file permissions: %w", err)
	}
	written, err := copySecureContent(file, source, options)
	if err != nil {
		return written, fmt.Errorf("write managed file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return written, fmt.Errorf("sync managed file: %w", err)
	}
	if options.Replace {
		err = unix.Renameat(parent, tmpName, parent, leaf)
	} else {
		err = unix.Renameat2(parent, tmpName, parent, leaf, unix.RENAME_NOREPLACE)
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			err = unix.Linkat(parent, tmpName, parent, leaf, 0)
			if err == nil {
				_ = unix.Unlinkat(parent, tmpName, 0)
			}
		}
	}
	if err != nil {
		return written, fmt.Errorf("commit managed file: %w", err)
	}
	committed = true
	// Persist the directory entry when the filesystem supports directory fsync.
	_ = unix.Fsync(parent)
	return written, nil
}

func validateManagedWriteTargetLinux(parent int, leaf string) error {
	fd, err := openatManagedLinux(parent, leaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return managedLinuxPathError("inspect managed write target", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("managed write target is a symlink: %w", ErrPathEscape)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("managed write target is not a regular file")
	}
	return nil
}

func removeDirectoryWithinNoFollow(root, relative string) error {
	parent, leaf, err := openManagedParentLinux(root, relative, false)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	fd, err := openatManagedLinux(parent, leaf, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return managedLinuxPathError("open managed directory for deletion", err)
	}
	_ = unix.Close(fd)
	if err := unix.Unlinkat(parent, leaf, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	_ = unix.Fsync(parent)
	return nil
}

func moveFileWithinNoFollow(root, sourceRelative, destinationRelative string) error {
	sourceParent, sourceLeaf, err := openManagedParentLinux(root, sourceRelative, false)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	sourceFD, err := openatManagedLinux(sourceParent, sourceLeaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return managedLinuxPathError("open managed move source", err)
	}
	var st unix.Stat_t
	defer unix.Close(sourceFD)
	statErr := unix.Fstat(sourceFD, &st)
	if statErr != nil {
		return statErr
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("managed move source is not a regular file")
	}
	destinationParent, destinationLeaf, err := openManagedParentLinux(root, destinationRelative, true)
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)
	err = unix.Renameat2(sourceParent, sourceLeaf, destinationParent, destinationLeaf, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		err = unix.Linkat(sourceParent, sourceLeaf, destinationParent, destinationLeaf, 0)
		if err == nil {
			if unlinkErr := unix.Unlinkat(sourceParent, sourceLeaf, 0); unlinkErr != nil {
				_ = unix.Unlinkat(destinationParent, destinationLeaf, 0)
				err = unlinkErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("move managed file: %w", err)
	}
	destinationFD, verifyErr := openatManagedLinux(destinationParent, destinationLeaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if verifyErr != nil {
		return managedLinuxPathError("verify managed move destination", verifyErr)
	}
	var destinationStat unix.Stat_t
	verifyErr = unix.Fstat(destinationFD, &destinationStat)
	_ = unix.Close(destinationFD)
	if verifyErr != nil || destinationStat.Dev != st.Dev || destinationStat.Ino != st.Ino {
		_ = unix.Unlinkat(destinationParent, destinationLeaf, 0)
		if verifyErr != nil {
			return fmt.Errorf("verify managed move destination: %w", verifyErr)
		}
		return errors.New("managed move source changed before commit")
	}
	_ = unix.Fsync(sourceParent)
	_ = unix.Fsync(destinationParent)
	return nil
}

func deleteManagedFileNoFollow(instancePath, cleaned string) error {
	parent, leaf, err := openManagedParentLinux(instancePath, cleaned, false)
	if err != nil {
		return err
	}
	defer unix.Close(parent)

	fd, err := openatManagedLinux(parent, leaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return managedLinuxPathError("open managed file for deletion", err)
	}
	var st unix.Stat_t
	statErr := unix.Fstat(fd, &st)
	_ = unix.Close(fd)
	if statErr != nil {
		return statErr
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return errors.New("recursive directory deletion is not allowed through file jobs")
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("managed file is a symlink: %w", ErrPathEscape)
	}
	// unlinkat never follows the final component. If an attacker swaps the leaf
	// after Fstat, at worst the replacement entry in this same held directory is
	// unlinked; it cannot redirect deletion outside the instance.
	if err := unix.Unlinkat(parent, leaf, 0); err != nil {
		return fmt.Errorf("delete managed file: %w", err)
	}
	_ = unix.Fsync(parent)
	return nil
}

func openManagedParentLinux(instancePath, cleaned string, create bool) (int, string, error) {
	parts, err := managedPathComponents(cleaned)
	if err != nil || len(parts) == 0 {
		return -1, "", ErrPathEscape
	}
	root, err := openManagedRootLinux(instancePath)
	if err != nil {
		return -1, "", err
	}
	current := root
	for _, component := range parts[:len(parts)-1] {
		next, openErr := openatManagedLinux(current, component, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(current, component, 0o750)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, "", fmt.Errorf("create managed directory %q: %w", component, mkdirErr)
			}
			next, openErr = openatManagedLinux(current, component, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(current)
			return -1, "", managedLinuxPathError("open managed directory", openErr)
		}
		_ = unix.Close(current)
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func openManagedRootLinux(instancePath string) (int, error) {
	abs, err := filepath.Abs(filepath.Clean(instancePath))
	if err != nil {
		return -1, err
	}
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_NONBLOCK | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, abs, how)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		fd, err = openManagedRootFallbackLinux(abs)
	}
	if err != nil {
		return -1, managedLinuxPathError("open instance root", err)
	}
	return fd, nil
}

func openManagedRootFallbackLinux(abs string) (int, error) {
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(current)
			return -1, ErrPathEscape
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(current)
			return -1, openErr
		}
		_ = unix.Close(current)
		current = next
	}
	return current, nil
}

func openatManagedLinux(parent int, name string, flags int, mode uint32) (int, error) {
	how := &unix.OpenHow{Flags: uint64(flags), Mode: uint64(mode), Resolve: managedLinuxResolveFlags}
	fd, err := unix.Openat2(parent, name, how)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return unix.Openat(parent, name, flags|unix.O_NOFOLLOW, mode)
	}
	return fd, err
}

func managedLinuxPathError(operation string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("%s refused a link or mount boundary: %w", operation, ErrPathEscape)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
