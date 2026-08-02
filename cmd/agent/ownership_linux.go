//go:build linux

package main

import (
	"fmt"
	"os/user"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
)

func prepareServiceConfigOwnership(path string) error {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	account, err := user.Lookup("l4d2panel")
	if err != nil {
		return fmt.Errorf("lookup l4d2panel service user: %w", err)
	}
	uid, gid, err := accountIDs(account)
	if err != nil {
		return err
	}
	if err := chownTreeWithoutLinks(cfg.DataDir, uid, gid); err != nil {
		return err
	}
	return nil
}
