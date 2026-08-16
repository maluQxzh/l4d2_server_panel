package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"
	"github.com/l4d2-panel/l4d2panel/internal/agent"
	"github.com/l4d2-panel/l4d2panel/internal/version"
)

const serviceName = "l4d2panel-agent"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "l4d2panel-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version" || args[0] == "-version") {
		fmt.Printf("%s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return nil
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runDirect(args)
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to agent YAML or JSON configuration")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	switch command {
	case "validate":
		cfg, err := agent.LoadConfig(absConfig)
		if err != nil {
			return err
		}
		a, err := agent.New(absConfig, cfg, log.New(os.Stderr, "agent validate: ", log.LstdFlags))
		if err != nil {
			return err
		}
		defer a.Close()
		fmt.Println("configuration is valid")
		return nil
	case "enroll":
		cfg, err := agent.LoadConfig(absConfig)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := agent.Enroll(ctx, absConfig, cfg); err != nil {
			return err
		}
		fmt.Printf("node enrolled: %s\n", cfg.NodeID)
		return nil
	case "run":
		return runService(absConfig)
	case "install":
		return installService(absConfig)
	case "uninstall", "start", "stop", "restart", "status":
		return controlService(command, absConfig)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: l4d2panel-agent [--config PATH] [--central-url URL] [--enrollment-token TOKEN] [--node-name NAME]\n       l4d2panel-agent <run|enroll|validate|install|uninstall|start|stop|restart|status|version> [--config PATH]\n\nRun without a command for portable foreground mode, or use install to register an auto-starting system service.")
}

// runDirect is the portable foreground entry point. It deliberately remains
// separate from install so a temporary portable launch never mutates the host's
// service manager. The explicit install command creates the service boundary.
func runDirect(args []string) error {
	flags := flag.NewFlagSet("direct", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", defaultDirectConfigPath(), "path to agent YAML or JSON configuration")
	centralURL := flags.String("central-url", os.Getenv("CENTRAL_URL"), "control-plane URL used for first enrollment")
	enrollmentToken := flags.String("enrollment-token", os.Getenv("ENROLLMENT_TOKEN"), "one-time token used for first enrollment")
	nodeName := flags.String("node-name", os.Getenv("NODE_NAME"), "node name used for first enrollment")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError()
	}
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	cfg, err := prepareDirectConfig(absConfig, *centralURL, *enrollmentToken, *nodeName)
	if err != nil {
		return err
	}
	if cfg.Credential == "" || cfg.NodeID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = agent.Enroll(ctx, absConfig, cfg)
		cancel()
		if err != nil {
			return fmt.Errorf("first enrollment: %w", err)
		}
		fmt.Printf("node enrolled: %s\n", cfg.NodeID)
	}
	return runService(absConfig)
}

func prepareDirectConfig(path, centralURL, enrollmentToken, nodeName string) (*agent.Config, error) {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		cfg = &agent.Config{}
	}

	enrolled := cfg.Credential != "" && cfg.NodeID != ""
	if enrolled {
		if centralURL != "" && strings.TrimRight(centralURL, "/") != strings.TrimRight(cfg.CentralURL, "/") {
			return nil, errors.New("--central-url does not match the control plane bound to this node")
		}
		if enrollmentToken != "" {
			return nil, errors.New("--enrollment-token is only valid before the node is enrolled")
		}
		return cfg, nil
	}

	if centralURL != "" {
		cfg.CentralURL = centralURL
	}
	if enrollmentToken != "" {
		cfg.EnrollmentToken = enrollmentToken
	}
	if nodeName != "" {
		cfg.NodeName = nodeName
	}
	if cfg.CentralURL == "" || cfg.EnrollmentToken == "" {
		return nil, fmt.Errorf("first start requires central_url and enrollment_token in %s, or --central-url and --enrollment-token", path)
	}
	// Rewriting the bootstrap input before the network request makes a manually
	// created Unix config 0600. Successful enrollment atomically replaces the
	// one-time token with the long-lived node credential in this same file.
	if err := agent.SaveConfig(path, cfg); err != nil {
		return nil, fmt.Errorf("save first-start config: %w", err)
	}
	return agent.LoadConfig(path)
}

func defaultDirectConfigPath() string {
	executable, err := os.Executable()
	if err != nil {
		return "agent.yaml"
	}
	return directConfigPath(executable)
}

func directConfigPath(executable string) string {
	return filepath.Join(filepath.Dir(executable), "agent.yaml")
}

type serviceProgram struct {
	configPath string
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
}

func (p *serviceProgram) Start(service.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	cfg, err := agent.LoadConfig(p.configPath)
	if err != nil {
		return err
	}
	logger, closer, err := agent.NewAgentLogger(cfg.DataDir)
	if err != nil {
		return err
	}
	a, err := agent.New(p.configPath, cfg, logger)
	if err != nil {
		logger.Printf("agent initialization failed: %v", err)
		closer.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		defer closer.Close()
		if err := a.Run(ctx); err != nil {
			logger.Printf("agent stopped with error: %v", err)
		}
	}()
	return nil
}

func (p *serviceProgram) Stop(service.Service) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel = nil
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("timed out stopping agent")
	}
}

func serviceConfiguration(configPath string) *service.Config {
	workingDirectory := filepath.Dir(configPath)
	if executable, err := os.Executable(); err == nil {
		workingDirectory = filepath.Dir(executable)
	}
	cfg := &service.Config{
		Name:             serviceName,
		DisplayName:      "L4D2 Panel Agent",
		Description:      "Manages local Left 4 Dead 2 dedicated server instances",
		Arguments:        []string{"run", "--config", configPath},
		WorkingDirectory: workingDirectory,
		Option: service.KeyValue{
			"Restart":                "always",
			"DelayedAutoStart":       true,
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "10s",
		},
	}
	if runtime.GOOS == "linux" {
		cfg.UserName = "l4d2panel"
		cfg.Dependencies = []string{"After=network-online.target", "Wants=network-online.target"}
	}
	if runtime.GOOS == "windows" {
		// A virtual service account has no reusable password, receives its own
		// service SID, and has substantially fewer local privileges than
		// LocalSystem. The installer grants that SID access only to panel data.
		cfg.UserName = `NT SERVICE\l4d2panel-agent`
	}
	return cfg
}

func newService(configPath string) (service.Service, error) {
	return service.New(&serviceProgram{configPath: configPath}, serviceConfiguration(configPath))
}

func runService(configPath string) error {
	if _, err := agent.LoadConfig(configPath); err != nil {
		return err
	}
	s, err := newService(configPath)
	if err != nil {
		return err
	}
	return s.Run()
}

func installService(configPath string) error {
	cfg, err := agent.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Credential == "" || cfg.NodeID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = agent.Enroll(ctx, configPath, cfg)
		cancel()
		if err != nil {
			return fmt.Errorf("enroll before service installation: %w", err)
		}
	}
	s, err := newService(configPath)
	if err != nil {
		return err
	}
	status, statusErr := s.Status()
	if statusErr == nil || !errors.Is(statusErr, service.ErrNotInstalled) {
		if err := validateExistingService(configPath); err != nil {
			if statusErr != nil {
				return fmt.Errorf("inspect existing Agent service: %v; validate ownership: %w", statusErr, err)
			}
			return err
		}
		if statusErr == nil && status == service.StatusRunning {
			delegated, err := ensureServicePrivileges([]string{"install", "--config", configPath})
			if err != nil || delegated {
				return err
			}
			if err := prepareServiceInstall(configPath, cfg); err != nil {
				return err
			}
			if err := prepareServiceConfigOwnership(configPath); err != nil {
				return err
			}
			fmt.Println("agent service is already installed and running; managed path permissions synchronized")
			return nil
		}
		return startInstalledService(s, configPath, cfg)
	}
	delegated, err := ensureServicePrivileges([]string{"install", "--config", configPath})
	if err != nil || delegated {
		return err
	}
	if err := prepareServiceInstall(configPath, cfg); err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		return err
	}
	// On Windows the virtual service SID becomes resolvable only after the
	// service exists. No service process is started before the ACL is applied.
	if err := prepareServiceConfigOwnership(configPath); err != nil {
		_ = s.Uninstall()
		return err
	}
	if err := s.Start(); err != nil {
		_ = s.Uninstall()
		return err
	}
	fmt.Println("agent service installed and started")
	return nil
}

func startInstalledService(s service.Service, configPath string, cfg *agent.Config) error {
	delegated, err := ensureServicePrivileges([]string{"install", "--config", configPath})
	if err != nil || delegated {
		return err
	}
	if err := prepareServiceInstall(configPath, cfg); err != nil {
		return err
	}
	if err := prepareServiceConfigOwnership(configPath); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	fmt.Println("agent service is installed, enabled, and running")
	return nil
}

func controlService(command, configPath string) error {
	if err := validateExistingService(configPath); err != nil {
		return err
	}
	if command != "status" {
		delegated, err := ensureServicePrivileges([]string{command, "--config", configPath})
		if err != nil || delegated {
			return err
		}
	}
	s, err := newService(configPath)
	if err != nil {
		return err
	}
	switch command {
	case "uninstall":
		_ = s.Stop()
		return s.Uninstall()
	case "start":
		return s.Start()
	case "stop":
		return s.Stop()
	case "restart":
		if err := s.Stop(); err != nil {
			return err
		}
		return s.Start()
	case "status":
		status, err := s.Status()
		if err != nil {
			return err
		}
		fmt.Println(status)
		return nil
	default:
		return usageError()
	}
}

func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if root := os.Getenv("ProgramData"); root != "" {
			return filepath.Join(root, "L4D2Panel", "agent.yaml")
		}
		return `C:\ProgramData\L4D2Panel\agent.yaml`
	}
	return "/etc/l4d2panel/agent.yaml"
}
