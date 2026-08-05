//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func createManagedHardLink(sourceRoot, sourceRelative, targetRoot, targetRelative string) error {
	sourceParent, sourceLeaf, err := openManagedParentLinux(sourceRoot, sourceRelative, false)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	targetParent, targetLeaf, err := openManagedParentLinux(targetRoot, targetRelative, true)
	if err != nil {
		return err
	}
	defer unix.Close(targetParent)

	sourceFD, err := openatManagedLinux(sourceParent, sourceLeaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return managedLinuxPathError("open map hard-link source", err)
	}
	defer unix.Close(sourceFD)
	var sourceStat unix.Stat_t
	if err := unix.Fstat(sourceFD, &sourceStat); err != nil {
		return err
	}
	if sourceStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("map hard-link source is not a regular file")
	}
	if err := unix.Linkat(sourceParent, sourceLeaf, targetParent, targetLeaf, 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create map hard link: %w", err)
	}
	targetFD, err := openatManagedLinux(targetParent, targetLeaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return managedLinuxPathError("verify map hard-link target", err)
	}
	defer unix.Close(targetFD)
	var targetStat unix.Stat_t
	if err := unix.Fstat(targetFD, &targetStat); err != nil {
		return err
	}
	if sourceStat.Dev != targetStat.Dev || sourceStat.Ino != targetStat.Ino || targetStat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("map hard-link target does not reference the expected source")
	}
	return nil
}

func managedHardLinkIdentity(root, relative string) (string, uint64, error) {
	parent, leaf, err := openManagedParentLinux(root, relative, false)
	if err != nil {
		return "", 0, err
	}
	defer unix.Close(parent)
	fd, err := openatManagedLinux(parent, leaf, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return "", 0, os.ErrNotExist
		}
		return "", 0, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", 0, errors.New("managed map link is not a regular file")
	}
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), stat.Ino), uint64(stat.Nlink), nil
}
