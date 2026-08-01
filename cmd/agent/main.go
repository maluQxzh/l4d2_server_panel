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
	if len(args) == 0 {
		return usageError()
	}
	command := args[0]
	if command == "version" || command == "--version" || command == "-version" {
		fmt.Printf("%s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return nil
	}
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
		if _, err := agent.New(absConfig, cfg, log.New(os.Stderr, "agent validate: ", log.LstdFlags)); err != nil {
			return err
		}
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
	return errors.New("usage: l4d2panel-agent <run|enroll|validate|install|uninstall|start|stop|restart|status|version> [--config PATH]")
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
	cfg := &service.Config{
		Name:        serviceName,
		DisplayName: "L4D2 Panel Agent",
		Description: "Manages local Left 4 Dead 2 dedicated server instances",
		Arguments:   []string{"run", "--config", configPath},
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

func controlService(command, configPath string) error {
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
