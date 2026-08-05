//go:build !windows

package agent

import (
	"fmt"
	"os"
	"syscall"
)

func mapFilesystemID(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errorsNewFilesystemIdentity(path)
	}
	return fmt.Sprintf("unix:%x", uint64(stat.Dev)), nil
}

func errorsNewFilesystemIdentity(path string) error {
	return fmt.Errorf("read filesystem identity for %s", path)
}
