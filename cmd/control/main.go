package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/containerupdate"
	"github.com/l4d2-panel/l4d2panel/internal/control"
	"github.com/l4d2-panel/l4d2panel/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := prepareContainerRuntimeIdentity(); err != nil {
		logger.Error("prepare container runtime identity", "error", err)
		os.Exit(1)
	}
	if err := runCLI(logger, os.Args[1:], os.Stdin); err != nil {
		logger.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func runCLI(logger *slog.Logger, args []string, stdin io.Reader) error {
	if len(args) == 0 {
		if runtime.GOOS == "windows" {
			return runPortableCommand(logger, []string{"install", "--interactive"}, stdin, os.Stdout)
		}
		return runConsole(logger, "")
	}
	switch args[0] {
	case "serve":
		configPath, err := parseConfigOnly("serve", args[1:])
		if err != nil {
			return err
		}
		if shouldRunAsService() {
			return runWindowsService(configPath)
		}
		return runConsole(logger, configPath)
	case "migrate":
		configPath, err := parseConfigOnly("migrate", args[1:])
		if err != nil {
			return err
		}
		return runMigrate(configPath)
	case "doctor":
		configPath, err := parseConfigOnly("doctor", args[1:])
		if err != nil {
			return err
		}
		return runDoctor(configPath, os.Stdout)
	case "backup-database":
		return runBackupDatabase(args[1:])
	case "bootstrap-owner":
		return runBootstrapOwner(logger, args[1:], stdin)
	case "setup-key":
		return runSetupKey(args[1:], os.Stdout)
	case "container-update-worker":
		return containerupdate.RunWorker(logger, args[1:])
	case "service":
		return runServiceCommand(args[1:])
	case "portable":
		return runPortableCommand(logger, args[1:], stdin, os.Stdout)
	case "version", "--version", "-version":
		fmt.Printf("%s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return nil
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: l4d2panel-control <serve|migrate|doctor|backup-database|bootstrap-owner|setup-key|service|portable|version> [options]")
}

func parseConfigOnly(command string, args []string) (string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to strict JSON configuration")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("%s does not accept positional arguments", command)
	}
	return absoluteConfigPath(*configPath)
}

func absoluteConfigPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return abs, nil
}

func loadConfig(path string) (control.Config, error) {
	if path == "" {
		return control.ConfigFromEnv()
	}
	return control.ConfigFromFile(path)
}

func runBootstrapOwner(logger *slog.Logger, args []string, stdin io.Reader) error {
	flags := flag.NewFlagSet("bootstrap-owner", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to strict JSON configuration")
	username := flags.String("username", "", "owner username")
	displayName := flags.String("display-name", "", "owner display name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bootstrap-owner does not accept positional arguments")
	}
	absConfig, err := absoluteConfigPath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(absConfig)
	if err != nil {
		return err
	}
	if *username == "" {
		*username = cfg.BootstrapUsername
	}
	if *displayName == "" {
		*displayName = cfg.BootstrapName
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
	store, err := control.OpenStore(ctx, cfg.DatabasePath)
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

func runSetupKey(args []string, output io.Writer) error {
	if len(args) == 0 || args[0] != "issue" {
		return errors.New("usage: l4d2panel-control setup-key issue [--config path]")
	}
	configPath, err := parseConfigOnly("setup-key issue", args[1:])
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key, err := control.IssueSetupKeyAtPath(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, key)
	return err
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

func runConsole(logger *slog.Logger, configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return serveControl(ctx, cfg, logger)
}

func serveControl(ctx context.Context, cfg control.Config, logger *slog.Logger) error {
	store, err := control.OpenStore(ctx, cfg.DatabasePath)
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

func runMigrate(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := control.OpenStore(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.Migrate(ctx, cfg.MigrationsDir)
}

type doctorResult struct {
	OK               bool   `json:"ok"`
	DatabaseEngine   string `json:"database_engine"`
	SQLiteVersion    string `json:"sqlite_version"`
	DatabasePath     string `json:"database_path"`
	DataDirectoryOK  bool   `json:"data_directory_writable"`
	LocalFilesystem  bool   `json:"local_filesystem"`
	ForeignKeys      bool   `json:"foreign_keys"`
	JournalMode      string `json:"journal_mode"`
	QuickCheck       string `json:"quick_check"`
	SchemaStatus     string `json:"schema_status"`
	ConfiguredListen string `json:"configured_listen"`
}

func runDoctor(configPath string, output io.Writer) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := control.OpenStore(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	sqliteVersion, err := store.SQLiteVersion(ctx)
	if err != nil {
		return err
	}
	foreignKeys, journalMode, err := store.SQLitePragmas(ctx)
	if err != nil {
		return err
	}
	if err := store.QuickCheck(ctx); err != nil {
		return err
	}
	schemaStatus, err := store.SchemaStatusForMigrations(ctx, cfg.MigrationsDir)
	if err != nil {
		return err
	}
	result := doctorResult{
		OK: true, DatabaseEngine: "sqlite", SQLiteVersion: sqliteVersion,
		DatabasePath: store.DatabasePath(), DataDirectoryOK: true, LocalFilesystem: true,
		ForeignKeys: foreignKeys, JournalMode: journalMode, QuickCheck: "ok",
		SchemaStatus: schemaStatus, ConfiguredListen: cfg.ListenAddress,
	}
	return json.NewEncoder(output).Encode(result)
}

func runBackupDatabase(args []string) error {
	flags := flag.NewFlagSet("backup-database", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to strict JSON configuration")
	output := flags.String("output", "", "new backup file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*output) == "" {
		return errors.New("backup-database requires --output FILE and accepts no positional arguments")
	}
	absConfig, err := absoluteConfigPath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(absConfig)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, err := control.OpenStore(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.BackupDatabase(ctx, *output)
}
