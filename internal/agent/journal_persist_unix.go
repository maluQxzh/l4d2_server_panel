//go:build !windows

package agent

import (
	"os"
	"path/filepath"
)

// replaceJournalFile makes both the new file contents and the directory entry
// durable before a job is allowed to proceed. Syncing only the temporary file
// is insufficient: a power loss after rename can otherwise resurrect the old
// journal and permit an already-claimed job to execute again.
func replaceJournalFile(temporary, destination string) error {
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func removeJournalFile(path, directoryPath string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	directory, err := os.Open(directoryPath)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
