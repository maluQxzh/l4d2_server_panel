//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
)

// prepareServiceConfigOwnership restricts the credential-bearing config to
// administrators, LocalSystem and this service's virtual account. Runtime
// roots receive inheritable Modify rights while the executable remains RX.
func prepareServiceConfigOwnership(path string) error {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	directories := []string{cfg.DataDir}
	if cfg.SteamCMDPath != "" {
		directories = append(directories, filepath.Dir(cfg.SteamCMDPath))
	}
	directories = append(directories, cfg.InstanceRoots...)
	directories = append(directories, cfg.MapRoots...)
	runtimePaths, err := agent.LoadRuntimeServicePaths(cfg.DataDir)
	if err != nil {
		return err
	}
	for _, installationPath := range runtimePaths.InstallationPaths {
		cleaned, validateErr := agent.ValidateServiceGameInstallation(installationPath)
		if validateErr != nil {
			return fmt.Errorf("refuse service ACL for registered game installation %s: %w", installationPath, validateErr)
		}
		directories = append(directories, cleaned)
	}
	seen := make(map[string]bool)
	for _, directory := range directories {
		if directory == "" {
			continue
		}
		cleaned, err := safeWindowsServiceDirectory(directory)
		if err != nil {
			return err
		}
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		if err := os.MkdirAll(cleaned, 0o700); err != nil {
			return err
		}
		if err := restrictedACL(cleaned, `(OI)(CI)M`); err != nil {
			return err
		}
	}
	if err := restrictedACL(path, `(M)`); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return restrictedACL(executable, `(RX)`)
}

func restrictedACL(path, serviceRights string) error {
	args := []string{
		path, "/inheritance:r", "/grant:r",
		`*S-1-5-18:(F)`,     // LocalSystem
		`*S-1-5-32-544:(F)`, // Built-in Administrators
		`NT SERVICE\l4d2panel-agent:` + serviceRights,
	}
	output, err := exec.Command("icacls.exe", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restrict ACL for %s: %w (%s)", path, err, output)
	}
	return nil
}
