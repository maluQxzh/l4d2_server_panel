//go:build windows

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Omitting FILE_SHARE_DELETE pins every opened directory/file object against
// rename or deletion for the lifetime of the security check and operation.
// FILE_SHARE_WRITE remains necessary for live srcds log/config handles and for
// NT's atomic rename path; relative operations stay anchored to pinned handles.
const (
	managedWindowsReadShare  = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	managedWindowsWriteShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
)

type managedWindowsRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func openManagedFileNoFollow(instancePath, cleaned string) (*os.File, error) {
	parent, leaf, err := openManagedParentWindows(instancePath, cleaned, false)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(parent)

	handle, err := openRelativeWindows(parent, leaf, windows.FILE_GENERIC_READ, managedWindowsReadShare, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, windows.FILE_ATTRIBUTE_NORMAL)
	if err != nil {
		return nil, managedWindowsPathError("open managed file", err)
	}
	if err := validateWindowsHandle(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), leaf)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create managed file handle")
	}
	return file, nil
}

func statManagedDirectoryNoFollow(instancePath, cleaned string) (os.FileInfo, error) {
	handle, err := openManagedDirectoryWindows(instancePath, cleaned)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), cleaned)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create managed directory handle")
	}
	defer file.Close()
	return file.Stat()
}

func listManagedDirectoryNoFollow(instancePath, cleaned string) ([]ManagedFile, error) {
	handle, err := openManagedDirectoryWindows(instancePath, cleaned)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(handle), cleaned)
	if directory == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create managed directory handle")
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	result := make([]ManagedFile, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || isReparseOrSymlink(info) {
			continue
		}
		result = append(result, ManagedFile{
			Name: entry.Name(), Path: filepath.ToSlash(filepath.Join(cleaned, entry.Name())), IsDir: info.IsDir(), Size: info.Size(), ModTime: info.ModTime(),
		})
	}
	return result, nil
}

func openManagedDirectoryWindows(instancePath, cleaned string) (windows.Handle, error) {
	handle, _, err := openManagedParentWindows(instancePath, filepath.ToSlash(filepath.Join(cleaned, "_managed_dir_anchor_")), false)
	return handle, err
}

func writeManagedFileNoFollow(instancePath, cleaned string, raw []byte) error {
	_, err := writeFileWithinNoFollow(instancePath, cleaned, bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0o640))
	return err
}

func writeManagedFileAtWindows(parent windows.Handle, leaf string, raw []byte) error {
	_, err := writeFileAtWindows(parent, leaf, bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0o640))
	return err
}

func writeFileWithinNoFollow(root, relative string, source io.Reader, options secureWriteOptions) (int64, error) {
	parent, leaf, err := openManagedParentWindows(root, relative, true)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(parent)
	return writeFileAtWindows(parent, leaf, source, options)
}

func writeFileAtWindows(parent windows.Handle, leaf string, source io.Reader, options secureWriteOptions) (int64, error) {
	if options.RejectExistingLink {
		if err := validateManagedWriteTargetWindows(parent, leaf); err != nil {
			return 0, err
		}
	}
	var (
		handle  windows.Handle
		tmpName string
		err     error
	)
	for attempt := 0; attempt < 8; attempt++ {
		tmpName, err = managedTempName()
		if err != nil {
			return 0, fmt.Errorf("create managed temporary name: %w", err)
		}
		handle, err = openRelativeWindows(parent, tmpName, windows.FILE_GENERIC_WRITE|windows.DELETE, managedWindowsWriteShare,
			windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_ATTRIBUTE_NORMAL)
		if err == windows.STATUS_OBJECT_NAME_COLLISION {
			continue
		}
		break
	}
	if err != nil {
		return 0, fmt.Errorf("create managed temporary file: %w", err)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return 0, errors.New("could not allocate a managed temporary file")
	}

	renamed := false
	file := os.NewFile(uintptr(handle), tmpName)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("create managed temporary file handle")
	}
	defer func() {
		if !renamed {
			_ = deleteWindowsHandle(handle)
		}
		_ = file.Close()
	}()
	_ = file.Chmod(options.Mode.Perm())
	written, err := copySecureContent(file, source, options)
	if err != nil {
		return written, fmt.Errorf("write managed file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return written, fmt.Errorf("sync managed file: %w", err)
	}
	if err := renameWindowsHandle(handle, parent, leaf, options.Replace); err != nil {
		return written, fmt.Errorf("commit managed file: %w", err)
	}
	renamed = true
	return written, nil
}

func validateManagedWriteTargetWindows(parent windows.Handle, leaf string) error {
	handle, err := openRelativeWindows(parent, leaf, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, managedWindowsReadShare,
		windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, windows.FILE_ATTRIBUTE_NORMAL)
	if isWindowsNotExist(err) {
		return nil
	}
	if err != nil {
		return managedWindowsPathError("inspect managed write target", err)
	}
	defer windows.CloseHandle(handle)
	return validateWindowsHandle(handle, false)
}

func removeDirectoryWithinNoFollow(root, relative string) error {
	parent, leaf, err := openManagedParentWindows(root, relative, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	handle, err := openRelativeWindows(parent, leaf, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		managedWindowsReadShare, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_ATTRIBUTE_DIRECTORY)
	if err != nil {
		return managedWindowsPathError("open managed directory for deletion", err)
	}
	defer windows.CloseHandle(handle)
	if err := validateWindowsHandle(handle, true); err != nil {
		return err
	}
	return deleteWindowsHandle(handle)
}

func moveFileWithinNoFollow(root, sourceRelative, destinationRelative string) error {
	sourceParent, sourceLeaf, err := openManagedParentWindows(root, sourceRelative, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(sourceParent)
	source, err := openRelativeWindows(sourceParent, sourceLeaf,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, managedWindowsReadShare, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_ATTRIBUTE_NORMAL)
	if err != nil {
		return managedWindowsPathError("open managed move source", err)
	}
	defer windows.CloseHandle(source)
	if err := validateWindowsHandle(source, false); err != nil {
		return err
	}
	destinationParent, destinationLeaf, err := openManagedParentWindows(root, destinationRelative, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(destinationParent)
	if err := renameWindowsHandle(source, destinationParent, destinationLeaf, false); err != nil {
		return fmt.Errorf("move managed file: %w", err)
	}
	return nil
}

func deleteManagedFileNoFollow(instancePath, cleaned string) error {
	parent, leaf, err := openManagedParentWindows(instancePath, cleaned, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)

	handle, err := openRelativeWindows(parent, leaf, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, managedWindowsReadShare,
		windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_ATTRIBUTE_NORMAL)
	if err != nil {
		return managedWindowsPathError("open managed file for deletion", err)
	}
	defer windows.CloseHandle(handle)
	if err := validateWindowsHandle(handle, false); err != nil {
		return err
	}
	if err := deleteWindowsHandle(handle); err != nil {
		return fmt.Errorf("delete managed file: %w", err)
	}
	return nil
}

func openManagedParentWindows(instancePath, cleaned string, create bool) (windows.Handle, string, error) {
	parts, err := managedPathComponents(cleaned)
	if err != nil || len(parts) == 0 {
		return 0, "", ErrPathEscape
	}
	for _, component := range parts {
		if !validWindowsManagedComponent(component) {
			return 0, "", ErrPathEscape
		}
	}
	current, err := openManagedRootWindows(instancePath, create)
	if err != nil {
		return 0, "", err
	}
	for _, component := range parts[:len(parts)-1] {
		access := uint32(windows.FILE_GENERIC_READ)
		share := uint32(managedWindowsReadShare)
		disposition := uint32(windows.FILE_OPEN)
		if create {
			access |= windows.FILE_GENERIC_WRITE
			share = managedWindowsWriteShare
			disposition = windows.FILE_OPEN_IF
		}
		next, openErr := openRelativeWindows(current, component, access, share, disposition,
			windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_ATTRIBUTE_DIRECTORY)
		if openErr != nil {
			_ = windows.CloseHandle(current)
			return 0, "", managedWindowsPathError("open managed directory", openErr)
		}
		if validateErr := validateWindowsHandle(next, true); validateErr != nil {
			_ = windows.CloseHandle(next)
			_ = windows.CloseHandle(current)
			return 0, "", validateErr
		}
		_ = windows.CloseHandle(current)
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func openManagedRootWindows(instancePath string, write bool) (windows.Handle, error) {
	abs, err := filepath.Abs(filepath.Clean(instancePath))
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(abs, `\\.\`) {
		return 0, ErrPathEscape
	}
	abs = stripWindowsExtendedPrefix(abs)
	volume := filepath.VolumeName(abs)
	if volume == "" {
		return 0, ErrPathEscape
	}
	var parts []string
	if relative := strings.TrimPrefix(abs[len(volume):], `\`); relative != "" {
		parts = strings.Split(relative, `\`)
	}
	for _, component := range parts {
		if !validWindowsManagedComponent(component) {
			return 0, ErrPathEscape
		}
	}
	access := uint32(windows.FILE_GENERIC_READ)
	share := uint32(managedWindowsReadShare)
	if write {
		access |= windows.FILE_GENERIC_WRITE
		share = managedWindowsWriteShare
	}
	// On Windows 10, OBJ_DONT_REPARSE on an absolute NT DOS path can
	// reject the drive's Object Manager link itself. Resolve only the volume
	// root through Win32, then open every filesystem component relative to a
	// pinned directory handle. Never retry a full path with no-follow disabled.
	volumePath, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return 0, err
	}
	ancestorAccess := uint32(windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	volumeAccess := ancestorAccess
	if len(parts) == 0 {
		volumeAccess = access
	}
	handle, err := windows.CreateFile(volumePath, volumeAccess, share, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, managedWindowsPathError("open managed volume root", err)
	}
	if err := validateWindowsHandle(handle, true); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	for i, component := range parts {
		componentAccess := ancestorAccess
		if i == len(parts)-1 {
			componentAccess = access
		}
		next, openErr := openRelativeWindows(handle, component, componentAccess, share, windows.FILE_OPEN,
			windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_ATTRIBUTE_DIRECTORY)
		if openErr != nil {
			_ = windows.CloseHandle(handle)
			return 0, managedWindowsPathError("open managed root component", openErr)
		}
		if validateErr := validateWindowsHandle(next, true); validateErr != nil {
			_ = windows.CloseHandle(next)
			_ = windows.CloseHandle(handle)
			return 0, validateErr
		}
		_ = windows.CloseHandle(handle)
		handle = next
	}
	finalPath, err := finalWindowsHandlePath(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("resolve managed root handle: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(finalPath), filepath.Clean(stripWindowsExtendedPrefix(abs))) {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("managed root handle resolved to an unexpected path: %w", ErrPathEscape)
	}
	return handle, nil
}

func finalWindowsHandlePath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 256)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return stripWindowsExtendedPrefix(windows.UTF16ToString(buffer[:length])), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func stripWindowsExtendedPrefix(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	}
	return strings.TrimPrefix(path, `\\?\`)
}

func openRelativeWindows(parent windows.Handle, name string, access, share, disposition, options, attributes uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var (
		handle windows.Handle
		iosb   windows.IO_STATUS_BLOCK
	)
	err = windows.NtCreateFile(&handle, access, oa, &iosb, nil, attributes, share,
		disposition, options, 0, 0)
	return handle, err
}

func validateWindowsHandle(handle windows.Handle, wantDirectory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect managed path: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("managed path is a reparse point: %w", ErrPathEscape)
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != wantDirectory {
		if isDirectory {
			return errors.New("recursive directory deletion is not allowed through file jobs")
		}
		return errors.New("managed directory component is not a directory")
	}
	return nil
}

func renameWindowsHandle(handle, parent windows.Handle, leaf string, replace bool) error {
	utf16Name, err := windows.UTF16FromString(leaf)
	if err != nil {
		return err
	}
	nameLength := (len(utf16Name) - 1) * 2
	var layout managedWindowsRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+nameLength)
	info := (*managedWindowsRenameInformation)(unsafe.Pointer(&buffer[0]))
	if replace {
		info.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS
	}
	info.RootDirectory = parent
	info.FileNameLength = uint32(nameLength)
	copy(unsafe.Slice(&info.FileName[0], len(utf16Name)-1), utf16Name[:len(utf16Name)-1])
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(handle, &iosb, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
}

func deleteWindowsHandle(handle windows.Handle) error {
	deleteOnClose := byte(1)
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(handle, &iosb, &deleteOnClose, 1, windows.FileDispositionInformation)
}

func validWindowsManagedComponent(component string) bool {
	if component == "" || strings.ContainsAny(component, "<>:\"/\\|?*") || strings.HasSuffix(component, " ") || strings.HasSuffix(component, ".") {
		return false
	}
	for _, r := range component {
		if r < 0x20 {
			return false
		}
	}
	base := strings.ToUpper(component)
	if before, _, found := strings.Cut(base, "."); found {
		base = before
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	default:
		return true
	}
}

func managedWindowsPathError(operation string, err error) error {
	if isWindowsNotExist(err) {
		return fmt.Errorf("%s: %w", operation, os.ErrNotExist)
	}
	if err == windows.STATUS_REPARSE_POINT_ENCOUNTERED || errors.Is(err, windows.ERROR_REPARSE_POINT_ENCOUNTERED) {
		return fmt.Errorf("%s refused a reparse point: %w", operation, ErrPathEscape)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func isWindowsNotExist(err error) bool {
	return err == windows.STATUS_OBJECT_NAME_NOT_FOUND || err == windows.STATUS_OBJECT_PATH_NOT_FOUND || err == windows.STATUS_NO_SUCH_FILE ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
