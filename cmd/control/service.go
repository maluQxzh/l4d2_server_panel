package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/kardianos/service"
	"github.com/l4d2-panel/l4d2panel/internal/control"
)

const controlServiceName = "l4d2panel-control"

type controlServiceProgram struct {
	configPath string
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
	eventLog   service.Logger
}

func (p *controlServiceProgram) Start(service.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	cfg, err := loadConfig(p.configPath)
	if err != nil {
		return err
	}
	logPath := cfg.LogFile
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(p.configPath), "logs", "control.jsonl")
	}
	writer, err := control.NewRotatingLog(logPath, 20<<20, 5)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		defer writer.Close()
		if err := serveControl(ctx, cfg, logger); err != nil {
			logger.Error("control service stopped", "error", err)
			if p.eventLog != nil {
				_ = p.eventLog.Error("Control service stopped: " + err.Error())
			}
			// A Windows service must terminate non-zero for SCM recovery actions
			// to run. Configuration and database errors are already in both the
			// bounded file log and Windows Event Log at this point.
			os.Exit(1)
		}
	}()
	return nil
}

func (p *controlServiceProgram) Stop(service.Service) error {
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
		return errors.New("timed out stopping control service")
	}
}

func controlServiceConfiguration(configPath string) *service.Config {
	return &service.Config{
		Name:        controlServiceName,
		DisplayName: "L4D2 Panel Control",
		Description: "Hosts the L4D2 Panel web control plane",
		Arguments:   []string{"serve", "--config", configPath},
		UserName:    `NT SERVICE\l4d2panel-control`,
		Option: service.KeyValue{
			"Restart":                "on-failure",
			"DelayedAutoStart":       true,
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "10s",
		},
	}
}

func newControlService(configPath string) (service.Service, error) {
	program := &controlServiceProgram{configPath: configPath}
	svc, err := service.New(program, controlServiceConfiguration(configPath))
	if err != nil {
		return nil, err
	}
	program.eventLog, _ = svc.Logger(nil)
	return svc, nil
}

func shouldRunAsService() bool {
	return runtime.GOOS == "windows" && !service.Interactive()
}

func runWindowsService(configPath string) error {
	if runtime.GOOS != "windows" {
		return errors.New("native Control service mode is supported only on Windows")
	}
	if configPath == "" {
		return errors.New("Windows service mode requires --config")
	}
	svc, err := newControlService(configPath)
	if err != nil {
		return err
	}
	return svc.Run()
}

func runServiceCommand(args []string) error {
	if runtime.GOOS != "windows" {
		return errors.New("control service management is supported only on Windows")
	}
	if len(args) == 0 {
		return errors.New("usage: l4d2panel-control service <install|uninstall|start|stop|restart|status> --config PATH")
	}
	action := args[0]
	flags := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", defaultControlConfigPath(), "path to strict JSON configuration")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("service command does not accept positional arguments")
	}
	absConfig, err := absoluteConfigPath(*configPath)
	if err != nil {
		return err
	}
	svc, err := newControlService(absConfig)
	if err != nil {
		return err
	}
	switch action {
	case "install":
		if _, err := control.ConfigFromFile(absConfig); err != nil {
			return err
		}
		if err := svc.Install(); err != nil {
			return err
		}
		fmt.Println("control service installed")
		return nil
	case "uninstall":
		_ = svc.Stop()
		return svc.Uninstall()
	case "start":
		return svc.Start()
	case "stop":
		return svc.Stop()
	case "restart":
		if err := svc.Stop(); err != nil {
			return err
		}
		return svc.Start()
	case "status":
		status, err := svc.Status()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, status)
		return nil
	default:
		return errors.New("unknown service action")
	}
}

func defaultControlConfigPath() string {
	if root := os.Getenv("ProgramData"); root != "" {
		return filepath.Join(root, "L4D2Panel", "config", "control.json")
	}
	return `C:\ProgramData\L4D2Panel\config\control.json`
}
