//go:build linux

package main

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

func prepareServiceConfigOwnership(path string) error {
	account, err := user.Lookup("l4d2panel")
	if err != nil {
		return fmt.Errorf("lookup l4d2panel service user: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("give agent config to l4d2panel: %w", err)
	}
	return os.Chmod(path, 0o600)
}
