package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")
var ErrUnauthorized = errors.New("unauthorized")

type Store struct {
	db                 *sql.DB
	lock               storeLock
	path               string
	projectionLocksMu  sync.Mutex
	projectionJobLocks map[string]*jobProjectionLock
}

type jobProjectionLock struct {
	mu   sync.Mutex
	refs int
}

// lockJobProjection serializes duplicate journal frames for one job without
// holding a database connection while the projector performs its own
// transactions. The database marker provides crash durability.
func (s *Store) lockJobProjection(id string) func() {
	s.projectionLocksMu.Lock()
	if s.projectionJobLocks == nil {
		s.projectionJobLocks = make(map[string]*jobProjectionLock)
	}
	lock := s.projectionJobLocks[id]
	if lock == nil {
		lock = &jobProjectionLock{}
		s.projectionJobLocks[id] = lock
	}
	lock.refs++
	s.projectionLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.projectionLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.projectionJobLocks, id)
		}
		s.projectionLocksMu.Unlock()
	}
}

func OpenStore(ctx context.Context, databasePath string) (*Store, error) {
	return openStore(ctx, databasePath, true)
}

// openSetupKeyStore opens the database without taking the Control process
// lock. It is intentionally limited to the local setup-key CLI, whose single
// immediate transaction must be able to run while Control is serving.
func openSetupKeyStore(ctx context.Context, databasePath string) (*Store, error) {
	return openStore(ctx, databasePath, false)
}

func openStore(ctx context.Context, databasePath string, takeProcessLock bool) (*Store, error) {
	databasePath = strings.TrimSpace(databasePath)
	if databasePath == "" {
		return nil, errors.New("database path is required")
	}
	absPath, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := validateLocalDatabasePath(absPath); err != nil {
		return nil, err
	}
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("secure database directory: %w", err)
	}
	var lock storeLock
	if takeProcessLock {
		lock, err = acquireStoreLock(absPath + ".lock")
		if err != nil {
			return nil, fmt.Errorf("acquire database lock: %w", err)
		}
	}

	query := url.Values{}
	query.Set("_busy_timeout", "10000")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "FULL")
	query.Set("_auto_vacuum", "incremental")
	query.Set("_txlock", "immediate")
	query.Set("_dqs", "false")
	query.Set("_time_integer_format", "unix_micro")
	query.Set("_inttotime", "1")
	query.Set("_timezone", "UTC")
	query.Add("_pragma", "trusted_schema(0)")
	query.Add("_pragma", "wal_autocheckpoint(1000)")
	query.Add("_pragma", "journal_size_limit(67108864)")
	uriPath := filepath.ToSlash(absPath)
	if filepath.VolumeName(absPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if lock != nil {
			_ = lock.Close()
		}
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := secureSQLiteFiles(absPath); err != nil {
		_ = db.Close()
		if lock != nil {
			_ = lock.Close()
		}
		return nil, fmt.Errorf("secure database file: %w", err)
	}
	return &Store{db: db, lock: lock, path: absPath}, nil
}

func (s *Store) Close() {
	if s.db != nil {
		if s.lock != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
			cancel()
		}
		_ = s.db.Close()
	}
	if s.lock != nil {
		_ = s.lock.Close()
	}
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) SQLiteVersion(ctx context.Context) (string, error) {
	var version string
	if err := s.db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&version); err != nil {
		return "", fmt.Errorf("read SQLite version: %w", err)
	}
	return version, nil
}

func (s *Store) DatabasePath() string { return s.path }

func (s *Store) SQLitePragmas(ctx context.Context) (bool, string, error) {
	var foreignKeys int
	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return false, "", err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return false, "", err
	}
	return foreignKeys == 1, strings.ToLower(journalMode), nil
}

func (s *Store) SchemaStatus(ctx context.Context) (string, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_migrations'`).Scan(&count)
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "uninitialized", nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		return "", err
	}
	if count == 0 {
		return "pending", nil
	}
	return "applied", nil
}

func (s *Store) SchemaStatusForMigrations(ctx context.Context, migrationsDir string) (string, error) {
	status, err := s.SchemaStatus(ctx)
	if err != nil || status != "applied" {
		return status, err
	}
	if strings.TrimSpace(migrationsDir) == "" {
		return "current", nil
	}
	migrationsDir, entries, err := resolveMigrationsDir(migrationsDir)
	if err != nil {
		return "", err
	}
	expected := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(migrationsDir, entry.Name()))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(body)
		expected[entry.Name()] = hex.EncodeToString(sum[:])
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version,checksum FROM schema_migrations`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return "", err
		}
		want, exists := expected[version]
		if !exists {
			return "", fmt.Errorf("database contains unknown migration %s", version)
		}
		if checksum != want {
			return "", fmt.Errorf("migration %s checksum changed", version)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if applied != len(expected) {
		return "pending", nil
	}
	return "current", nil
}

func (s *Store) QuickCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check failed: %s", result)
	}
	return nil
}

// BackupDatabase creates a standalone, consistent SQLite snapshot. OpenStore's
// process lock guarantees that Control is stopped while this method runs.
func (s *Store) BackupDatabase(ctx context.Context, output string) error {
	if err := s.QuickCheck(ctx); err != nil {
		return err
	}
	absOutput, err := filepath.Abs(strings.TrimSpace(output))
	if err != nil || strings.TrimSpace(output) == "" {
		return errors.New("backup output path is required")
	}
	if filepath.Clean(absOutput) == filepath.Clean(s.path) {
		return errors.New("backup output must differ from database path")
	}
	if _, err := os.Stat(absOutput); err == nil {
		return fmt.Errorf("backup output already exists: %s", absOutput)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absOutput), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(absOutput), ".l4d2panel-backup-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	type backuper interface {
		NewBackup(string) (*sqlite.Backup, error)
	}
	if err := conn.Raw(func(driverConn any) error {
		backup, err := driverConn.(backuper).NewBackup(tmpPath)
		if err != nil {
			return err
		}
		for more := true; more; {
			more, err = backup.Step(256)
			if err != nil {
				_ = backup.Finish()
				return err
			}
		}
		return backup.Finish()
	}); err != nil {
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	file, err := os.OpenFile(tmpPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, absOutput); err != nil {
		return fmt.Errorf("commit SQLite backup: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context, migrationsDir string) error {
	migrationsDir, entries, err := resolveMigrationsDir(migrationsDir)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMP NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	knownChecksums := make(map[string]string, len(names))
	for _, name := range names {
		body, readErr := os.ReadFile(filepath.Join(migrationsDir, name))
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", name, readErr)
		}
		sum := sha256.Sum256(body)
		knownChecksums[name] = hex.EncodeToString(sum[:])
	}
	appliedRows, err := s.db.QueryContext(ctx, `SELECT version,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	for appliedRows.Next() {
		var version, checksum string
		if err := appliedRows.Scan(&version, &checksum); err != nil {
			_ = appliedRows.Close()
			return err
		}
		expected, exists := knownChecksums[version]
		if !exists {
			_ = appliedRows.Close()
			return fmt.Errorf("database contains unknown migration %s", version)
		}
		if checksum != expected {
			_ = appliedRows.Close()
			return fmt.Errorf("migration %s checksum changed", version)
		}
	}
	if err := appliedRows.Err(); err != nil {
		_ = appliedRows.Close()
		return err
	}
	if err := appliedRows.Close(); err != nil {
		return err
	}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=?1`, name).Scan(&existing)
		if err == nil {
			_ = tx.Rollback()
			if existing != checksum {
				return fmt.Errorf("migration %s checksum changed", name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum,applied_at) VALUES(?1,?2,?3)`, name, checksum, time.Now().UTC())
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func resolveMigrationsDir(migrationsDir string) (string, []os.DirEntry, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err == nil {
		return migrationsDir, entries, nil
	}
	if !filepath.IsAbs(migrationsDir) {
		if executable, execErr := os.Executable(); execErr == nil {
			candidate := filepath.Join(filepath.Dir(executable), migrationsDir)
			if alt, altErr := os.ReadDir(candidate); altErr == nil {
				return candidate, alt, nil
			}
		}
	}
	return "", nil, fmt.Errorf("read migrations: %w", err)
}

func rowsAffected(result sql.Result) (int64, error) { return result.RowsAffected() }

func resultRowsAffected(result sql.Result) int64 {
	n, _ := result.RowsAffected()
	return n
}

func isSQLiteConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 19
}

func (s *Store) MaintainSQLite(ctx context.Context) error {
	for _, statement := range []string{
		`PRAGMA optimize`,
		`PRAGMA incremental_vacuum(200)`,
		`PRAGMA wal_checkpoint(PASSIVE)`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func redactDatabaseError(err error, databaseLocation string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	message = strings.ReplaceAll(message, databaseLocation, "[redacted database]")
	if parsed, parseErr := url.Parse(databaseLocation); parseErr == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			message = strings.ReplaceAll(message, password, "[redacted]")
			message = strings.ReplaceAll(message, url.QueryEscape(password), "[redacted]")
		}
	}
	return errors.New(message)
}

func normalizeUsername(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
