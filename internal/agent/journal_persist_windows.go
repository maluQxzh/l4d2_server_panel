//go:build windows

package agent

import "golang.org/x/sys/windows"

// Windows does not provide a portable directory fsync. MoveFileEx with
// WRITE_THROUGH is the supported replacement primitive which waits for the
// move to reach durable storage before returning.
func replaceJournalFile(temporary, destination string) error {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
