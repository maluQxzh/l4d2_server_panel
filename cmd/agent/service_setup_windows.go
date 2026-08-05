//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func ensureServicePrivileges(arguments []string) (bool, error) {
	if windows.GetCurrentProcessToken().IsElevated() {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(executable)
	parameters, _ := syscall.UTF16PtrFromString(joinWindowsArguments(arguments))
	directory, _ := syscall.UTF16PtrFromString(filepath.Dir(executable))
	result, _, callErr := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW").Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(parameters)),
		uintptr(unsafe.Pointer(directory)), 1)
	if result <= 32 {
		return false, fmt.Errorf("administrator elevation was canceled or failed: %v", callErr)
	}
	return true, nil
}

func validateExistingService(configPath string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+serviceName, registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("read existing Agent Windows service: %w", err)
	}
	defer key.Close()
	imagePath, _, err := key.GetStringValue("ImagePath")
	if err != nil {
		return err
	}
	account, _, err := key.GetStringValue("ObjectName")
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(imagePath), strings.ToLower(executable)) ||
		!strings.Contains(strings.ToLower(imagePath), strings.ToLower(configPath)) ||
		!strings.EqualFold(account, `NT SERVICE\l4d2panel-agent`) {
		return fmt.Errorf("the existing %s Windows service is not owned by this portable deployment", serviceName)
	}
	return nil
}

func joinWindowsArguments(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = syscall.EscapeArg(argument)
	}
	return strings.Join(quoted, " ")
}

func prepareServiceInstall(_ string, cfg *agent.Config) error {
	directories := append([]string{cfg.DataDir}, cfg.InstanceRoots...)
	directories = append(directories, cfg.MapRoots...)
	seen := make(map[string]bool, len(directories))
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
			return fmt.Errorf("create Agent service directory %s: %w", cleaned, err)
		}
	}
	return nil
}

func safeWindowsServiceDirectory(directory string) (string, error) {
	cleaned := filepath.Clean(directory)
	if cleaned == filepath.VolumeName(cleaned)+string(filepath.Separator) {
		return "", fmt.Errorf("refusing broad Agent service directory %s", cleaned)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if relative, relErr := filepath.Rel(cleaned, executable); relErr == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing Agent service directory %s because it contains the Agent binary", cleaned)
	}
	return cleaned, nil
}
