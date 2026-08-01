package control

import (
	"context"
	"crypto/sha256"
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")
var ErrUnauthorized = errors.New("unauthorized")

type Store struct {
	pool               *pgxpool.Pool
	projectionLocksMu  sync.Mutex
	projectionJobLocks map[string]*jobProjectionLock
}

type jobProjectionLock struct {
	mu   sync.Mutex
	refs int
}

// lockJobProjection serializes duplicate journal frames for one job without
// holding a database connection while the projector performs its own
// transactions. V1 intentionally runs a single Control process; durability
// across process restarts comes from the database marker, while this keyed lock
// handles in-process WebSocket concurrency.
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

func OpenStore(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", redactDatabaseError(err, databaseURL))
	}
	config.MaxConns = 20
	config.MinConns = 2
	config.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", redactDatabaseError(err, databaseURL))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", redactDatabaseError(err, databaseURL))
	}
	return &Store{pool: pool}, nil
}

func redactDatabaseError(err error, databaseURL string) error {
	if err == nil {
		return nil
	}
	message := strings.ReplaceAll(err.Error(), databaseURL, "[database URL redacted]")
	if parsed, parseErr := url.Parse(databaseURL); parseErr == nil && parsed.User != nil {
		if password, present := parsed.User.Password(); present && password != "" {
			message = strings.ReplaceAll(message, password, "[redacted]")
			message = strings.ReplaceAll(message, url.QueryEscape(password), "[redacted]")
		}
	}
	return errors.New(message)
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// DatabaseServerVersion returns PostgreSQL's numeric server version without
// exposing connection details. Windows installer diagnostics use this to fail
// before attempting migrations against an unsupported major version.
func (s *Store) DatabaseServerVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.pool.QueryRow(ctx, "SHOW server_version_num").Scan(&version); err != nil {
		return 0, fmt.Errorf("read PostgreSQL server version: %w", err)
	}
	return version, nil
}

// CheckMigrationPrivileges verifies that the configured role can create
// objects in the target schema. The probe is transaction-scoped and rolled
// back, so doctor never mutates a healthy database.
func (s *Store) CheckMigrationPrivileges(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration privilege probe: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "CREATE TABLE public.__l4d2panel_doctor_probe (id integer)"); err != nil {
		return fmt.Errorf("database role cannot create migration objects in public schema: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context, migrationsDir string) error {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		if !filepath.IsAbs(migrationsDir) {
			if executable, execErr := os.Executable(); execErr == nil {
				candidate := filepath.Join(filepath.Dir(executable), migrationsDir)
				if alt, altErr := os.ReadDir(candidate); altErr == nil {
					migrationsDir, entries, err = candidate, alt, nil
				}
			}
		}
		if err != nil {
			return fmt.Errorf("read migrations: %w", err)
		}
	}
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1 FOR UPDATE`, name).Scan(&existing)
		if err == nil {
			_ = tx.Rollback(ctx)
			if existing != checksum {
				return fmt.Errorf("migration %s checksum changed", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return err
		}
		if _, err = tx.Exec(ctx, string(body)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, name, checksum)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func normalizeUsername(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
