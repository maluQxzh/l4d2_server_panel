package control

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const setupKeyPrefix = "l4d2p_setup_"

var (
	ErrBootstrapComplete     = errors.New("bootstrap is already complete")
	ErrSetupKeyNotConfigured = errors.New("setup key is not configured")
	ErrInvalidSetupKey       = errors.New("setup key is invalid")
)

func ValidSetupKey(value string) bool {
	if !strings.HasPrefix(value, setupKeyPrefix) || len(value) != len(setupKeyPrefix)+43 {
		return false
	}
	_, err := decodeRawURLToken(strings.TrimPrefix(value, setupKeyPrefix), 32)
	return err == nil
}

func decodeRawURLToken(value string, size int) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != size {
		return nil, errors.New("invalid token")
	}
	return raw, nil
}

// IssueSetupKeyAtPath is the only maintenance operation allowed to bypass the
// single-Control process lock. It performs one bounded immediate transaction
// and never returns a general-purpose Store to callers.
func IssueSetupKeyAtPath(ctx context.Context, databasePath string) (string, error) {
	store, err := openSetupKeyStore(ctx, databasePath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	return store.IssueSetupKey(ctx)
}

func (s *Store) IssueSetupKey(ctx context.Context) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	raw := setupKeyPrefix + token
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var users int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		return "", err
	}
	if users != 0 {
		return "", ErrBootstrapComplete
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bootstrap_setup_key(singleton,key_hash,created_at)
		VALUES(1,?1,?2) ON CONFLICT(singleton) DO UPDATE SET key_hash=excluded.key_hash,created_at=excluded.created_at`,
		tokenHash(raw), time.Now().UTC()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Store) SetupKeyConfigured(ctx context.Context) (bool, error) {
	var configured bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bootstrap_setup_key WHERE singleton=1)`).Scan(&configured)
	return configured, err
}

func (s *Store) ValidateSetupKey(ctx context.Context, raw string) error {
	var stored []byte
	err := s.db.QueryRowContext(ctx, `SELECT key_hash FROM bootstrap_setup_key WHERE singleton=1`).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSetupKeyNotConfigured
	}
	if err != nil {
		return err
	}
	provided := tokenHash(raw)
	if len(stored) != len(provided) || subtle.ConstantTimeCompare(stored, provided) != 1 {
		return ErrInvalidSetupKey
	}
	return nil
}

func (s *Store) BootstrapOwnerWithSetupKey(ctx context.Context, raw, passwordHash string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback()
	var users int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		return domain.User{}, err
	}
	if users != 0 {
		return domain.User{}, ErrBootstrapComplete
	}
	var stored []byte
	if err := tx.QueryRowContext(ctx, `SELECT key_hash FROM bootstrap_setup_key WHERE singleton=1`).Scan(&stored); errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, ErrSetupKeyNotConfigured
	} else if err != nil {
		return domain.User{}, err
	}
	provided := tokenHash(raw)
	if len(stored) != len(provided) || subtle.ConstantTimeCompare(stored, provided) != 1 {
		return domain.User{}, ErrInvalidSetupKey
	}
	user := domain.User{ID: uuid.NewString(), Username: "admin", DisplayName: "系统管理员", Role: domain.RoleOwner,
		AccountState: domain.AccountActive, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,is_bootstrap_owner,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,true,?7)`, user.ID, user.Username, normalizeUsername(user.Username), user.DisplayName,
		passwordHash, user.Role, user.CreatedAt); err != nil {
		return domain.User{}, fmt.Errorf("create bootstrap owner: %w", err)
	}
	if err := insertRoleBindings(ctx, tx, user.ID, user.ID, []domain.RoleBinding{{Role: domain.RoleOwner, ScopeType: "global", Permissions: presetPermissions(domain.RoleOwner)}}); err != nil {
		return domain.User{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bootstrap_setup_key WHERE singleton=1`); err != nil {
		return domain.User{}, err
	}
	return user, tx.Commit()
}
