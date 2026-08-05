//go:build !linux && !windows

package agent

import "errors"

func createManagedHardLink(string, string, string, string) error {
	return errors.New("managed map hard links are supported only on Linux and Windows")
}

func managedHardLinkIdentity(string, string) (string, uint64, error) {
	return "", 0, errors.New("managed map hard links are supported only on Linux and Windows")
}
