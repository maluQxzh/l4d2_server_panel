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
// roots give inheritable Full control to LocalSystem/Administrators and
// inheritable Modify rights to the service while files keep non-inheritable
// rights. Making every trusted directory ACE inheritable is important because
// the service creates logs, journals and other descendants after installation.
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
		if err := restrictedDirectoryACL(cleaned, `(OI)(CI)M`); err != nil {
			return err
		}
	}
	if err := restrictedFileACL(path, `(M)`); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return restrictedFileACL(executable, `(RX)`)
}

func restrictedDirectoryACL(path, serviceRights string) error {
	return applyRestrictedACL(path, restrictedDirectoryACLArguments(path, serviceRights))
}

func restrictedFileACL(path, serviceRights string) error {
	return applyRestrictedACL(path, restrictedFileACLArguments(path, serviceRights))
}

func restrictedDirectoryACLArguments(path, serviceRights string) []string {
	return restrictedACLArguments(path, `(OI)(CI)F`, serviceRights)
}

func restrictedFileACLArguments(path, serviceRights string) []string {
	return restrictedACLArguments(path, `(F)`, serviceRights)
}

func restrictedACLArguments(path, trustedRights, serviceRights string) []string {
	return []string{
		path, "/inheritance:r", "/grant:r",
		`*S-1-5-18:` + trustedRights,     // LocalSystem
		`*S-1-5-32-544:` + trustedRights, // Built-in Administrators
		`NT SERVICE\l4d2panel-agent:` + serviceRights,
	}
}

func applyRestrictedACL(path string, args []string) error {
	output, err := exec.Command("icacls.exe", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restrict ACL for %s: %w (%s)", path, err, output)
	}
	return nil
}
