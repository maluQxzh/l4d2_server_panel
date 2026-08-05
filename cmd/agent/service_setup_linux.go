//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
)

func ensureServicePrivileges(arguments []string) (bool, error) {
	if os.Geteuid() == 0 {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return false, errors.New("installing or controlling the systemd service requires root; rerun this command with sudo")
	}
	command := exec.Command(sudo, append([]string{"--", executable}, arguments...)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return false, fmt.Errorf("run elevated service command: %w", err)
	}
	return true, nil
}

func validateExistingService(configPath string) error {
	raw, err := os.ReadFile("/etc/systemd/system/" + serviceName + ".service")
	if err != nil {
		return fmt.Errorf("read existing Agent systemd unit: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	unit := string(raw)
	expectedExec := "ExecStart=" + strings.ReplaceAll(executable, " ", `\x20`) + ` "run" "--config" "` +
		strings.ReplaceAll(configPath, `"`, `\"`) + `"`
	if !linePresent(unit, expectedExec) || !linePresent(unit, "User=l4d2panel") {
		return errors.New("the existing l4d2panel-agent systemd unit is not owned by this portable deployment")
	}
	return nil
}

func linePresent(content, expected string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == expected {
			return true
		}
	}
	return false
}

func prepareServiceInstall(configPath string, cfg *agent.Config) error {
	if os.Geteuid() != 0 {
		return errors.New("installing the systemd service requires root")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd/systemctl is required for automatic background startup")
	}
	if info, err := os.Stat("/run/systemd/system"); err != nil || !info.IsDir() {
		return errors.New("systemd is not the active init system; use portable foreground mode or another process manager")
	}
	account, err := user.Lookup("l4d2panel")
	if err != nil {
		if _, lookupErr := exec.LookPath("useradd"); lookupErr != nil {
			return errors.New("the l4d2panel service user is missing and useradd is unavailable")
		}
		shell := "/usr/sbin/nologin"
		if _, statErr := os.Stat(shell); statErr != nil {
			shell = "/sbin/nologin"
			if _, statErr := os.Stat(shell); statErr != nil {
				shell = "/bin/false"
			}
		}
		output, createErr := exec.Command("useradd", "--system", "--user-group", "--home-dir", cfg.DataDir,
			"--no-create-home", "--shell", shell, "l4d2panel").CombinedOutput()
		if createErr != nil {
			return fmt.Errorf("create l4d2panel service user: %w: %s", createErr, strings.TrimSpace(string(output)))
		}
		account, err = user.Lookup("l4d2panel")
	}
	if err != nil {
		return fmt.Errorf("lookup l4d2panel service user: %w", err)
	}
	uid, gid, err := accountIDs(account)
	if err != nil {
		return err
	}
	if uid <= 0 || uid >= 1000 {
		return fmt.Errorf("refusing non-system l4d2panel account with uid %d", uid)
	}
	if err := grantServiceDirectoryAccess(filepath.Dir(configPath), gid); err != nil {
		return err
	}
	if executable, executableErr := os.Executable(); executableErr != nil {
		return executableErr
	} else if err := grantServiceDirectoryAccess(filepath.Dir(executable), gid); err != nil {
		return err
	}
	if err := verifyExecutableReachable(uid, gid); err != nil {
		return err
	}
	allowedMapLinks, err := agent.ManagedMapHardLinks(cfg)
	if err != nil {
		return fmt.Errorf("validate registered map hard links: %w", err)
	}
	managed := append([]string{cfg.DataDir}, cfg.InstanceRoots...)
	managed = append(managed, cfg.MapRoots...)
	registeredPaths, err := registeredServiceGamePaths(cfg)
	if err != nil {
		return err
	}
	managed = append(managed, registeredPaths...)
	registered := make(map[string]bool, len(registeredPaths))
	for _, path := range registeredPaths {
		registered[path] = true
	}
	seen := make(map[string]bool, len(managed))
	for _, path := range managed {
		cleaned, err := safeManagedServicePath(path)
		if err != nil {
			return err
		}
		if seen[cleaned] || pathWithinAny(cleaned, seen) {
			continue
		}
		if err := os.MkdirAll(cleaned, 0o750); err != nil {
			return fmt.Errorf("create Agent service directory %s: %w", cleaned, err)
		}
		if err := chownTreeAllowingManagedLinks(cleaned, uid, gid, allowedMapLinks); err != nil {
			return err
		}
		if registered[cleaned] {
			fmt.Printf("adopted registered game installation %s for the l4d2panel service account\n", cleaned)
		}
		seen[cleaned] = true
	}
	if err := os.Chown(configPath, uid, gid); err != nil {
		return fmt.Errorf("give Agent config to l4d2panel: %w", err)
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return err
	}
	return nil
}

func registeredServiceGamePaths(cfg *agent.Config) ([]string, error) {
	runtimePaths, err := agent.LoadRuntimeServicePaths(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(runtimePaths.InstallationPaths))
	for _, installationPath := range runtimePaths.InstallationPaths {
		cleaned, validateErr := agent.ValidateServiceGameInstallation(installationPath)
		if validateErr != nil {
			return nil, fmt.Errorf("refuse service ownership for registered game installation %s: %w", installationPath, validateErr)
		}
		cleaned, validateErr = safeManagedServicePath(cleaned)
		if validateErr != nil {
			return nil, validateErr
		}
		paths = append(paths, cleaned)
	}
	return paths, nil
}

func accountIDs(account *user.User) (int, int, error) {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func safeManagedServicePath(path string) (string, error) {
	cleaned, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if cleaned == string(filepath.Separator) {
		return "", errors.New("refusing to change service ownership of the filesystem root")
	}
	relative := strings.Trim(filepath.ToSlash(cleaned), "/")
	if relative == "" || !strings.Contains(relative, "/") {
		return "", fmt.Errorf("refusing broad Agent service directory %s; use a dedicated nested directory", cleaned)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if relativeExecutable, relErr := filepath.Rel(cleaned, executable); relErr == nil && relativeExecutable != ".." &&
		!strings.HasPrefix(relativeExecutable, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing Agent service directory %s because it contains the Agent binary", cleaned)
	}
	return cleaned, nil
}

func pathWithinAny(path string, parents map[string]bool) bool {
	for parent := range parents {
		relative, err := filepath.Rel(parent, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func chownTreeWithoutLinks(root string, uid, gid int) error {
	return chownTreeAllowingManagedLinks(root, uid, gid, nil)
}

func chownTreeAllowingManagedLinks(root string, uid, gid int, allowedMapLinks map[string]bool) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("read filesystem identity for %s", root)
	}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing linked Agent service path %s", path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("read filesystem identity for %s", path)
		}
		if stat.Dev != rootStat.Dev {
			return fmt.Errorf("refusing mounted filesystem inside Agent service path %s", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing special file inside Agent service path %s", path)
		}
		if info.Mode().IsRegular() && stat.Nlink > 1 && !allowedMapLinks[filepath.Clean(path)] {
			return fmt.Errorf("refusing hard-linked file inside Agent service path %s", path)
		}
		return nil
	}); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing linked Agent service path %s", path)
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("set Agent service ownership on %s: %w", path, err)
		}
		return nil
	})
}

func grantServiceDirectoryAccess(path string, gid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Agent service parent is not a plain directory: %s", path)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("set Agent service group on %s: %w", path, err)
	}
	// Preserve owner/other access, remove group write, and grant the dedicated
	// service group only the read/traverse rights needed to reach fixed files.
	mode := (info.Mode().Perm() & 0o705) | 0o050
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("grant Agent service traversal on %s: %w", path, err)
	}
	return nil
}

func verifyExecutableReachable(uid, gid int) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		return fmt.Errorf("make Agent binary executable: %w", err)
	}
	for current := filepath.Dir(executable); current != string(filepath.Separator); current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		mode := info.Mode().Perm()
		reachable := mode&0o001 != 0 || (int(stat.Uid) == uid && mode&0o100 != 0) || (int(stat.Gid) == gid && mode&0o010 != 0)
		if !reachable {
			return fmt.Errorf("Agent service user cannot traverse %s; move the portable directory to a service-readable location", current)
		}
	}
	return nil
}
