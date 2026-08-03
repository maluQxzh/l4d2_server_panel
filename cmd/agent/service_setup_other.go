//go:build !linux && !windows

package main

import (
	"errors"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
)

func ensureServicePrivileges([]string) (bool, error) {
	return false, errors.New("automatic service installation is supported only on Linux and Windows")
}

func prepareServiceInstall(string, *agent.Config) error {
	return errors.New("automatic service installation is supported only on Linux and Windows")
}

func validateExistingService(string) error {
	return errors.New("automatic service installation is supported only on Linux and Windows")
}
