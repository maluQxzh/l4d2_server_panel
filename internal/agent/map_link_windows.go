//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type managedWindowsLinkInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func createManagedHardLink(sourceRoot, sourceRelative, targetRoot, targetRelative string) error {
	sourceParent, sourceLeaf, err := openManagedParentWindows(sourceRoot, sourceRelative, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(sourceParent)
	targetParent, targetLeaf, err := openManagedParentWindows(targetRoot, targetRelative, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(targetParent)
	source, err := openRelativeWindows(sourceParent, sourceLeaf, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		managedWindowsReadShare, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, windows.FILE_ATTRIBUTE_NORMAL)
	if err != nil {
		return managedWindowsPathError("open map hard-link source", err)
	}
	defer windows.CloseHandle(source)
	if err := validateWindowsHandle(source, false); err != nil {
		return err
	}
	utf16Name, err := windows.UTF16FromString(targetLeaf)
	if err != nil {
		return err
	}
	nameLength := (len(utf16Name) - 1) * 2
	var layout managedWindowsLinkInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+nameLength)
	info := (*managedWindowsLinkInformation)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = targetParent
	info.FileNameLength = uint32(nameLength)
	copy(unsafe.Slice(&info.FileName[0], len(utf16Name)-1), utf16Name[:len(utf16Name)-1])
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtSetInformationFile(source, &iosb, &buffer[0], uint32(len(buffer)), windows.FileLinkInformation)
	if err != nil && err != windows.STATUS_OBJECT_NAME_COLLISION {
		return fmt.Errorf("create map hard link: %w", err)
	}
	target, err := openRelativeWindows(targetParent, targetLeaf, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		managedWindowsReadShare, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, windows.FILE_ATTRIBUTE_NORMAL)
	if err != nil {
		return managedWindowsPathError("verify map hard-link target", err)
	}
	defer windows.CloseHandle(target)
	if err := validateWindowsHandle(target, false); err != nil {
		return err
	}
	sourceID, _, err := windowsHardLinkIdentity(source)
	if err != nil {
		return err
	}
	targetID, _, err := windowsHardLinkIdentity(target)
	if err != nil {
		return err
	}
	if sourceID != targetID {
		return errors.New("map hard-link target does not reference the expected source")
	}
	return nil
}

func managedHardLinkIdentity(root, relative string) (string, uint64, error) {
	parent, leaf, err := openManagedParentWindows(root, relative, false)
	if err != nil {
		return "", 0, err
	}
	defer windows.CloseHandle(parent)
	handle, err := openRelativeWindows(parent, leaf, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		managedWindowsReadShare, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, windows.FILE_ATTRIBUTE_NORMAL)
	if isWindowsNotExist(err) {
		return "", 0, os.ErrNotExist
	}
	if err != nil {
		return "", 0, err
	}
	defer windows.CloseHandle(handle)
	return windowsHardLinkIdentity(handle)
}

func windowsHardLinkIdentity(handle windows.Handle) (string, uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", 0, err
	}
	identity := fmt.Sprintf("%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
	return identity, uint64(info.NumberOfLinks), nil
}
