package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/control"
	"github.com/l4d2-panel/l4d2panel/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var err error
	switch {
	case len(os.Args) == 1:
		err = run(logger)
	case os.Args[1] == "serve":
		err = run(logger)
	case os.Args[1] == "bootstrap-owner":
		err = runBootstrapOwner(logger, os.Args[2:], os.Stdin)
	default:
		err = fmt.Errorf("unknown command %q (expected serve or bootstrap-owner)", os.Args[1])
	}
	if err != nil {
		logger.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func runBootstrapOwner(logger *slog.Logger, args []string, stdin io.Reader) error {
	cfg, err := control.ConfigFromEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("bootstrap-owner", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	username := flags.String("username", cfg.BootstrapUsername, "owner username")
	displayName := flags.String("display-name", cfg.BootstrapName, "owner display name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bootstrap-owner does not accept positional arguments")
	}
	password, err := readBootstrapPassword(stdin)
	if err != nil {
		return err
	}
	hash, err := control.HashPassword(password)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := control.OpenStore(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx, cfg.MigrationsDir); err != nil {
		return err
	}
	needsBootstrap, err := store.NeedsBootstrap(ctx)
	if err != nil {
		return err
	}
	if !needsBootstrap {
		return errors.New("an owner already exists; use the authenticated team management API instead")
	}
	user, created, err := store.BootstrapOwner(ctx, *username, *displayName, hash)
	if err != nil {
		return err
	}
	if !created {
		return errors.New("owner was not created because bootstrap is already complete")
	}
	logger.Info("bootstrap owner created", "username", user.Username)
	return nil
}

func readBootstrapPassword(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("bootstrap password must be provided on standard input")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 1027))
	if err != nil {
		return "", err
	}
	if len(raw) == 1027 {
		return "", errors.New("bootstrap password exceeds 1024 bytes")
	}
	password := strings.TrimSuffix(string(raw), "\n")
	password = strings.TrimSuffix(password, "\r")
	if len(password) > 1024 {
		return "", errors.New("bootstrap password exceeds 1024 bytes")
	}
	if strings.ContainsAny(password, "\r\n\x00") {
		return "", errors.New("bootstrap password must be a single line without NUL bytes")
	}
	return password, nil
}

func run(logger *slog.Logger) error {
	cfg, err := control.ConfigFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := control.OpenStore(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx, cfg.MigrationsDir); err != nil {
		return err
	}
	needsBootstrap, err := store.NeedsBootstrap(ctx)
	if err != nil {
		return err
	}
	if needsBootstrap {
		password := cfg.BootstrapPassword
		if password == "" {
			logger.Warn("no owner exists; open the web interface to complete first-run setup", "setup_path", "/setup")
		} else {
			hash, err := control.HashPassword(password)
			if err != nil {
				return err
			}
			user, created, err := store.BootstrapOwner(ctx, cfg.BootstrapUsername, cfg.BootstrapName, hash)
			if err != nil {
				return err
			}
			if created {
				logger.Info("bootstrap owner created from legacy environment", "username", user.Username)
			}
		}
	}

	app := control.NewServer(cfg, store, logger)
	defer app.Close()
	go app.RunHousekeeping(ctx)
	httpServer := &http.Server{
		Addr: cfg.ListenAddress, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 75 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	serverError := make(chan error, 1)
	go func() {
		logger.Info("control plane listening", "address", cfg.ListenAddress, "version", version.Version)
		serverError <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-serverError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
