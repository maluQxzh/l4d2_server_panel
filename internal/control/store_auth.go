package control

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type userAuthRecord struct {
	User         domain.User
	PasswordHash string
	TOTPSecret   string
	Disabled     bool
}

type Principal struct {
	User            domain.User
	Bindings        []domain.RoleBinding
	BindingsLoaded  bool
	SessionID       string
	CSRFHash        []byte
	ExpiresAt       time.Time
	SessionKind     string
	APITokenID      string
	TokenScopes     []string
	TokenNodeID     string
	TokenInstanceID string
}

func (p Principal) IsAPIToken() bool { return p.APITokenID != "" }

// Allows applies both the user's current role and the narrower API-token
// grants. API tokens can never exercise owner-only security operations.
func (p Principal) Allows(permission Permission) bool {
	allowed := false
	if len(p.Bindings) == 0 && !p.BindingsLoaded {
		allowed = RoleAllows(p.User.Role, permission)
	} else {
		for _, binding := range p.Bindings {
			if bindingAllows(binding, permission) {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		return false
	}
	if !p.IsAPIToken() {
		return true
	}
	if permission == PermissionRootSecurity {
		return false
	}
	return tokenScopesAllow(p.TokenScopes, permission)
}

func (p Principal) GlobalAllows(permission Permission) bool {
	roleAllowed := false
	if len(p.Bindings) == 0 && !p.BindingsLoaded {
		roleAllowed = RoleAllows(p.User.Role, permission)
	} else {
		for _, binding := range p.Bindings {
			if binding.ScopeType == "global" && bindingAllows(binding, permission) {
				roleAllowed = true
				break
			}
		}
	}
	if !roleAllowed || (p.IsAPIToken() && permission == PermissionRootSecurity) {
		return false
	}
	if p.IsAPIToken() {
		return !p.HasTokenResourceRestriction() && tokenScopesAllow(p.TokenScopes, permission)
	}
	return true
}

func (s *Store) NeedsBootstrap(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

func (s *Store) BootstrapOwner(ctx context.Context, username, displayName, passwordHash string) (domain.User, bool, error) {
	// The DSN makes every explicit transaction BEGIN IMMEDIATE. The single
	// SQLite writer lock therefore serializes the empty check and owner insert.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, false, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return domain.User{}, false, err
	}
	if count > 0 {
		return domain.User{}, false, tx.Commit()
	}
	user := domain.User{ID: uuid.NewString(), Username: strings.TrimSpace(username), DisplayName: strings.TrimSpace(displayName), Role: domain.RoleOwner,
		AccountState: domain.AccountActive, CreatedAt: time.Now().UTC()}
	if user.Username == "" {
		user.Username = "admin"
	}
	if user.DisplayName == "" {
		user.DisplayName = user.Username
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,is_bootstrap_owner,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,true,?7)`, user.ID, user.Username, normalizeUsername(user.Username), user.DisplayName, passwordHash, user.Role, user.CreatedAt)
	if err != nil {
		return domain.User{}, false, err
	}
	if err = insertRoleBindings(ctx, tx, user.ID, user.ID, []domain.RoleBinding{{Role: domain.RoleOwner, ScopeType: "global", Permissions: presetPermissions(domain.RoleOwner)}}); err != nil {
		return domain.User{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bootstrap_setup_key WHERE singleton=1`); err != nil {
		return domain.User{}, false, err
	}
	return user, true, tx.Commit()
}

func scanAuthUser(row scanner) (userAuthRecord, error) {
	var r userAuthRecord
	err := row.Scan(&r.User.ID, &r.User.Username, &r.User.DisplayName, &r.User.Role, &r.User.TOTPEnabled,
		&r.User.CreatedAt, &r.User.LastLoginAt, &r.PasswordHash, &r.TOTPSecret, &r.Disabled, &r.User.BootstrapOwner, &r.User.AccountState)
	r.User.Disabled = r.Disabled
	return r, err
}

func (s *Store) UserForLogin(ctx context.Context, username string) (userAuthRecord, error) {
	r, err := scanAuthUser(s.db.QueryRowContext(ctx, `SELECT id,username,display_name,role,totp_enabled,created_at,last_login_at,
		password_hash,COALESCE(totp_secret,''),disabled,is_bootstrap_owner,account_state FROM users WHERE username_normalized=?1`, normalizeUsername(username)))
	if errors.Is(err, sql.ErrNoRows) {
		return userAuthRecord{}, ErrNotFound
	}
	return r, err
}

func (s *Store) CreateSession(ctx context.Context, userID string, tokenDigest, csrfDigest []byte, remoteIP, userAgent string, expiresAt time.Time) (string, error) {
	return s.CreateSessionWithKind(ctx, userID, tokenDigest, csrfDigest, remoteIP, userAgent, expiresAt, "standard")
}

func (s *Store) CreateSessionWithKind(ctx context.Context, userID string, tokenDigest, csrfDigest []byte, remoteIP, userAgent string, expiresAt time.Time, kind string) (string, error) {
	id := uuid.NewString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_hash,remote_ip,user_agent,expires_at,kind)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8)`, id, userID, tokenDigest, csrfDigest, remoteIP, userAgent, expiresAt, kind)
	if err != nil {
		return "", err
	}
	if kind == "standard" {
		_, _ = s.db.ExecContext(ctx, `UPDATE users SET last_login_at=unixepoch('subsec')*1000000 WHERE id=?1`, userID)
	}
	return id, nil
}

func (s *Store) Session(ctx context.Context, tokenDigest []byte) (Principal, error) {
	var p Principal
	now := time.Now().UTC()
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role,u.totp_enabled,u.created_at,u.last_login_at,u.is_bootstrap_owner,u.account_state,
		s.id,s.csrf_hash,s.expires_at,s.kind FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=?1 AND s.expires_at>?2 AND NOT u.disabled`, tokenDigest, now).Scan(
		&p.User.ID, &p.User.Username, &p.User.DisplayName, &p.User.Role, &p.User.TOTPEnabled,
		&p.User.CreatedAt, &p.User.LastLoginAt, &p.User.BootstrapOwner, &p.User.AccountState, &p.SessionID, &p.CSRFHash, &p.ExpiresAt, &p.SessionKind)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err == nil {
		err = s.loadPrincipalBindings(ctx, &p)
	}
	if err == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at=?2 WHERE id=?1 AND last_seen_at<?3`, p.SessionID, now, now.Add(-5*time.Minute))
	}
	return p, err
}

func (s *Store) ActiveSessionByID(ctx context.Context, sessionID string) (Principal, error) {
	var p Principal
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role,u.totp_enabled,u.created_at,u.last_login_at,u.is_bootstrap_owner,u.account_state,
		s.id,s.csrf_hash,s.expires_at,s.kind FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.id=?1 AND s.expires_at>?2 AND NOT u.disabled`, sessionID, time.Now().UTC()).Scan(
		&p.User.ID, &p.User.Username, &p.User.DisplayName, &p.User.Role, &p.User.TOTPEnabled,
		&p.User.CreatedAt, &p.User.LastLoginAt, &p.User.BootstrapOwner, &p.User.AccountState, &p.SessionID, &p.CSRFHash, &p.ExpiresAt, &p.SessionKind)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err == nil {
		err = s.loadPrincipalBindings(ctx, &p)
	}
	return p, err
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?1`, sessionID)
	return err
}

func (s *Store) RotateSessionCSRF(ctx context.Context, sessionID string, digest []byte) error {
	tag, err := s.db.ExecContext(ctx, `UPDATE sessions SET csrf_hash=?2 WHERE id=?1`, sessionID, digest)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SaveTOTPSecret(ctx context.Context, userID, encrypted string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET totp_secret=?2,totp_enabled=false WHERE id=?1`, userID, encrypted)
	return err
}

func (s *Store) TOTPSecret(ctx context.Context, userID string) (string, bool, error) {
	var secret string
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(totp_secret,''),totp_enabled FROM users WHERE id=?1`, userID).Scan(&secret, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return secret, enabled, err
}

func (s *Store) EnableTOTP(ctx context.Context, userID string) error {
	tag, err := s.db.ExecContext(ctx, `UPDATE users SET totp_enabled=true WHERE id=?1 AND totp_secret IS NOT NULL`, userID)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

// EnableTOTPWithRecoveryCodes replaces every older recovery code and enables
// TOTP in one transaction. A failed insert can therefore never leave an
// account enabled without a usable recovery path.
func (s *Store) EnableTOTPWithRecoveryCodes(ctx context.Context, userID string, codeDigests [][]byte) error {
	if len(codeDigests) == 0 {
		return errors.New("at least one recovery code is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tag, err := tx.ExecContext(ctx, `UPDATE users SET totp_enabled=true WHERE id=?1 AND totp_secret IS NOT NULL`, userID)
	if err != nil {
		return err
	}
	if resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_recovery_codes WHERE user_id=?1`, userID); err != nil {
		return err
	}
	for _, digest := range codeDigests {
		if len(digest) == 0 {
			return errors.New("recovery code digest is empty")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO totp_recovery_codes(id,user_id,code_hash) VALUES(?1,?2,?3)`,
			uuid.NewString(), userID, digest); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConsumeTOTPRecoveryCode uses a single conditional UPDATE so concurrent
// login attempts can never successfully reuse the same recovery code.
func (s *Store) ConsumeTOTPRecoveryCode(ctx context.Context, userID string, digest []byte) (bool, error) {
	tag, err := s.db.ExecContext(ctx, `UPDATE totp_recovery_codes SET used_at=unixepoch('subsec')*1000000
		WHERE user_id=?1 AND code_hash=?2 AND used_at IS NULL`, userID, digest)
	if err != nil {
		return false, err
	}
	return resultRowsAffected(tag) == 1, nil
}
