//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/kardianos/service"
	"github.com/l4d2-panel/l4d2panel/internal/control"
	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
	"github.com/l4d2-panel/l4d2panel/internal/version"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const portablePurgeConfirmation = "DELETE L4D2PANEL PORTABLE DATA"

var serviceSIDPattern = regexp.MustCompile(`S-1-5-80(?:-[0-9]+){5}`)

func runPortableCommand(logger *slog.Logger, args []string, stdin io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: l4d2panel-control portable <install|start|stop|restart|status|upgrade|unregister|purge>")
	}
	switch args[0] {
	case "install":
		flags := flag.NewFlagSet("portable install", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		interactive := flags.Bool("interactive", false, "show first-run result and wait before closing")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: l4d2panel-control portable install [--interactive]")
		}
		if !windows.GetCurrentProcessToken().IsElevated() {
			return relaunchElevated(append([]string{"portable", "install"}, boolArgument(*interactive)...))
		}
		return portableInstall(logger, *interactive, stdin, output)
	case "start", "stop", "restart", "status":
		if len(args) != 1 {
			return errors.New("portable service actions do not accept additional arguments")
		}
		if args[0] != "status" && !windows.GetCurrentProcessToken().IsElevated() {
			return relaunchElevated([]string{"portable", args[0]})
		}
		return portableServiceAction(args[0])
	case "upgrade":
		if len(args) != 2 {
			return errors.New("usage: l4d2panel-control portable upgrade PACKAGE.zip")
		}
		if !windows.GetCurrentProcessToken().IsElevated() {
			absolutePackage, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			return relaunchElevated([]string{"portable", "upgrade", absolutePackage})
		}
		return portableUpgrade(args[1], output)
	case "upgrade-worker":
		flags := flag.NewFlagSet("portable upgrade-worker", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		root := flags.String("root", "", "portable root")
		jobID := flags.String("job", "", "update job ID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *root == "" || *jobID == "" {
			return errors.New("invalid internal upgrade-worker arguments")
		}
		return portableupdate.RunWorker(*root, *jobID)
	case "unregister":
		if !windows.GetCurrentProcessToken().IsElevated() {
			return relaunchElevated([]string{"portable", "unregister"})
		}
		return portableUnregister(output)
	case "purge":
		flags := flag.NewFlagSet("portable purge", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		confirmation := flags.String("confirmation", "", "exact destructive confirmation")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *confirmation != portablePurgeConfirmation {
			return fmt.Errorf("portable purge requires --confirmation %q", portablePurgeConfirmation)
		}
		if !windows.GetCurrentProcessToken().IsElevated() {
			return relaunchElevated([]string{"portable", "purge", "--confirmation", *confirmation})
		}
		if err := portableUnregister(output); err != nil {
			return err
		}
		root, err := portableRoot()
		if err != nil {
			return err
		}
		data := filepath.Join(root, "data")
		if filepath.Dir(data) != root {
			return errors.New("refusing to purge an unresolved data path")
		}
		if err := os.RemoveAll(data); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Portable data was permanently removed.")
		return err
	default:
		return errors.New("unknown portable command")
	}
}

func portableInstall(logger *slog.Logger, interactive bool, stdin io.Reader, output io.Writer) error {
	root, err := portableRoot()
	if err != nil {
		return err
	}
	if err := validatePortableLocation(root); err != nil {
		return err
	}
	configPath := filepath.Join(root, "data", "config", "control.json")
	for _, relative := range []string{"data/config", "data/database", "data/logs", "data/backups/upgrades", "data/update"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(relative)), 0o700); err != nil {
			return err
		}
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if err := writePortableConfig(root, configPath); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := control.ConfigFromFile(configPath); err != nil {
		return err
	}
	if conflict, imagePath, err := conflictingService(root); err != nil {
		return err
	} else if conflict {
		return fmt.Errorf("service %s belongs to another deployment (%s); uninstall the legacy MSI first", controlServiceName, imagePath)
	}
	svc, err := newControlService(configPath)
	if err != nil {
		return err
	}
	serviceInstalled := false
	if status, statusErr := svc.Status(); statusErr == nil {
		serviceInstalled = true
		if status != service.StatusStopped {
			if err := svc.Stop(); err != nil {
				return err
			}
		}
	} else if !errors.Is(statusErr, service.ErrNotInstalled) {
		return statusErr
	}
	if err := runMigrate(configPath); err != nil {
		return err
	}
	if err := runDoctor(configPath, io.Discard); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := control.OpenStore(ctx, filepath.Join(root, "data", "database", "l4d2panel.sqlite3"))
	if err != nil {
		return err
	}
	needsBootstrap, err := store.NeedsBootstrap(ctx)
	store.Close()
	if err != nil {
		return err
	}
	if !serviceInstalled {
		if err := svc.Install(); err != nil {
			return err
		}
	}
	if err := grantPortableACL(root); err != nil {
		return err
	}
	if err := grantServiceSelfControl(); err != nil {
		return err
	}
	if err := configurePortableFirewall(); err != nil {
		return err
	}
	if err := runServiceCommand([]string{"start", "--config", configPath}); err != nil {
		return err
	}
	if err := waitPortableReady(90 * time.Second); err != nil {
		return err
	}
	setupKey := ""
	if needsBootstrap {
		keyContext, keyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer keyCancel()
		setupKey, err = control.IssueSetupKeyAtPath(keyContext, filepath.Join(root, "data", "database", "l4d2panel.sqlite3"))
		if err != nil {
			return err
		}
	}
	logger.Info("Windows portable Control installed", "root", root, "port", 19090)
	fmt.Fprintln(output, "L4D2 Panel 中心端已启动：http://127.0.0.1:19090")
	fmt.Fprintln(output, "请先配置用户自行管理的 HTTPS 反向代理，再开放浏览器访问。")
	if setupKey != "" {
		fmt.Fprintln(output, "初始化密钥（同时是 admin 初始密码，只显示这一次）：")
		fmt.Fprintln(output, setupKey)
	}
	if interactive {
		fmt.Fprintln(output, "按 Enter 键关闭窗口……")
		_, _ = fmt.Fscanln(stdin)
	}
	return nil
}

func portableUpgrade(zipPath string, output io.Writer) error {
	root, err := portableRoot()
	if err != nil {
		return err
	}
	if err := validatePortableLocation(root); err != nil {
		return err
	}
	if conflict, imagePath, err := conflictingService(root); err != nil {
		return err
	} else if conflict {
		return fmt.Errorf("service %s belongs to another deployment (%s); uninstall the legacy MSI first", controlServiceName, imagePath)
	}
	if !serviceRegistryExists() {
		return errors.New("portable Control service is not registered; run portable install first")
	}
	manager := portableupdate.Detect(filepath.Join(root, "data", "config", "control.json"), filepath.Join(root, "data", "database", "l4d2panel.sqlite3"), version.Version, true)
	state, err := manager.CreateFromFile(zipPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "升级任务已启动：%s\n", state.JobID)
	fmt.Fprintln(output, "当前命令将退出以释放 EXE；请在设置页查看服务恢复后的升级结果。")
	return nil
}

func portableServiceAction(action string) error {
	root, err := portableRoot()
	if err != nil {
		return err
	}
	if conflict, imagePath, err := conflictingService(root); err != nil {
		return err
	} else if conflict {
		return fmt.Errorf("refusing to control service owned by another deployment: %s", imagePath)
	}
	if !serviceRegistryExists() {
		return errors.New("portable Control service is not registered; run portable install first")
	}
	return runServiceCommand([]string{action, "--config", defaultControlConfigPath()})
}

func portableUnregister(output io.Writer) error {
	root, err := portableRoot()
	if err != nil {
		return err
	}
	if conflict, imagePath, err := conflictingService(root); err != nil {
		return err
	} else if conflict {
		return fmt.Errorf("refusing to remove service owned by another deployment: %s", imagePath)
	}
	config := defaultControlConfigPath()
	svc, err := newControlService(config)
	if err != nil {
		return err
	}
	status, err := svc.Status()
	if err == nil {
		if status != service.StatusStopped {
			if err := svc.Stop(); err != nil {
				return err
			}
		}
		if err := svc.Uninstall(); err != nil {
			return err
		}
		deadline := time.Now().Add(30 * time.Second)
		for serviceRegistryExists() && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if serviceRegistryExists() {
			return errors.New("timed out removing Control service")
		}
	} else if !errors.Is(err, service.ErrNotInstalled) {
		return err
	}
	_ = runSystem("netsh.exe", "advfirewall", "firewall", "delete", "rule", "name=L4D2 Panel Center TCP 19090")
	_, writeErr := fmt.Fprintln(output, "Control service and TCP 19090 firewall rule were removed. Portable data was preserved.")
	return writeErr
}

func serviceRegistryExists() bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+controlServiceName, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	key.Close()
	return true
}

func writePortableConfig(root, configPath string) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	config := map[string]any{
		"schema_version": 2, "listen_address": "0.0.0.0:19090",
		"database_path":  filepath.Join(root, "data", "database", "l4d2panel.sqlite3"),
		"migrations_dir": filepath.Join(root, "migrations", "sqlite"),
		"master_key":     base64.StdEncoding.EncodeToString(key), "cookie_secure": true,
		"session_ttl": "12h", "public_url": "", "trusted_proxies": []string{"127.0.0.1/32", "::1/128"},
		"timezone": "Asia/Hong_Kong", "log_file": filepath.Join(root, "data", "logs", "control.jsonl"),
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	temp := configPath + ".tmp"
	if err := os.WriteFile(temp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, configPath)
}

func portableRoot() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		resolved = executable
	}
	return filepath.Abs(filepath.Dir(resolved))
}

func validatePortableLocation(root string) error {
	if strings.EqualFold(os.Getenv("PROCESSOR_ARCHITECTURE"), "ARM64") || strings.EqualFold(os.Getenv("PROCESSOR_ARCHITEW6432"), "ARM64") {
		return errors.New("portable Control currently supports Windows x64, not Windows on ARM")
	}
	osVersion := windows.RtlGetVersion()
	if osVersion.MajorVersion < 10 || osVersion.BuildNumber < 17763 {
		return errors.New("portable Control requires Windows 10 or Windows Server 2019 and later")
	}
	if strings.HasPrefix(root, `\\`) {
		return errors.New("portable Control cannot run from a UNC or network path")
	}
	temp, _ := filepath.Abs(os.TempDir())
	if strings.EqualFold(root, temp) || strings.HasPrefix(strings.ToLower(root), strings.ToLower(temp)+string(os.PathSeparator)) {
		return errors.New("portable Control cannot be installed in the Windows temporary directory")
	}
	volume := filepath.VolumeName(root)
	if len(volume) != 2 || volume[1] != ':' {
		return errors.New("portable Control requires a local fixed drive")
	}
	rootPath, _ := syscall.UTF16PtrFromString(volume + `\`)
	result, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW").Call(uintptr(unsafe.Pointer(rootPath)))
	if result != 3 {
		return errors.New("portable Control requires a local fixed drive")
	}
	return nil
}

func conflictingService(root string) (bool, string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+controlServiceName, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	defer key.Close()
	imagePath, _, err := key.GetStringValue("ImagePath")
	if err != nil {
		return false, "", err
	}
	account, _, err := key.GetStringValue("ObjectName")
	if err != nil {
		return false, "", err
	}
	executable := filepath.Join(root, "l4d2panel-control.exe")
	config := filepath.Join(root, "data", "config", "control.json")
	owned := strings.Contains(strings.ToLower(imagePath), strings.ToLower(executable)) &&
		strings.Contains(strings.ToLower(imagePath), strings.ToLower(config)) && strings.EqualFold(account, `NT SERVICE\l4d2panel-control`)
	return !owned, imagePath, nil
}

func grantPortableACL(root string) error {
	return runSystem(filepath.Join(os.Getenv("SystemRoot"), "System32", "icacls.exe"), root, "/inheritance:r", "/grant:r",
		"*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/grant", `NT SERVICE\l4d2panel-control:(OI)(CI)M`)
}

func grantServiceSelfControl() error {
	if err := runSystem(filepath.Join(os.Getenv("SystemRoot"), "System32", "sc.exe"), "sidtype", controlServiceName, "unrestricted"); err != nil {
		return err
	}
	output, err := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "sc.exe"), "showsid", controlServiceName).CombinedOutput()
	if err != nil {
		return err
	}
	sid := serviceSIDPattern.FindString(string(output))
	if sid == "" {
		return errors.New("could not resolve Control service SID")
	}
	output, err = exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "sc.exe"), "sdshow", controlServiceName).CombinedOutput()
	if err != nil {
		return err
	}
	sddl := strings.TrimSpace(string(output))
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return errors.New("could not read Control service security descriptor")
	}
	sddl = sddl[start:]
	ace := "(A;;RPWP;;;" + sid + ")"
	if strings.Contains(sddl, ace) {
		return nil
	}
	if sacl := strings.Index(sddl, "S:"); sacl >= 0 {
		sddl = sddl[:sacl] + ace + sddl[sacl:]
	} else {
		sddl += ace
	}
	return runSystem(filepath.Join(os.Getenv("SystemRoot"), "System32", "sc.exe"), "sdset", controlServiceName, sddl)
}

func configurePortableFirewall() error {
	_ = runSystem("netsh.exe", "advfirewall", "firewall", "delete", "rule", "name=L4D2 Panel Center TCP 19090")
	return runSystem("netsh.exe", "advfirewall", "firewall", "add", "rule", "name=L4D2 Panel Center TCP 19090", "dir=in", "action=allow", "protocol=TCP", "localport=19090", "profile=any")
}

func runSystem(name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func waitPortableReady(timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://127.0.0.1:19090/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("Control readiness check timed out")
}

func relaunchElevated(arguments []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(executable)
	parameters, _ := syscall.UTF16PtrFromString(joinWindowsArguments(arguments))
	directory, _ := syscall.UTF16PtrFromString(filepath.Dir(executable))
	result, _, callErr := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW").Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(parameters)), uintptr(unsafe.Pointer(directory)), 1)
	if result <= 32 {
		return fmt.Errorf("administrator elevation was canceled or failed: %v", callErr)
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

func boolArgument(value bool) []string {
	if value {
		return []string{"--interactive"}
	}
	return nil
}
