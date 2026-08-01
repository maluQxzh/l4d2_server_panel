package control

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
			if RoleAllows(binding.Role, permission) {
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
			if binding.ScopeType == "global" && RoleAllows(binding.Role, permission) {
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
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

func (s *Store) BootstrapOwner(ctx context.Context, username, displayName, passwordHash string) (domain.User, bool, error) {
	// Read Committed is intentional: a transaction waiting for the advisory
	// lock must take a fresh snapshot after the preceding bootstrap commits.
	// A Serializable snapshot could be fixed before the wait and still observe
	// an empty users table afterwards.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return domain.User{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('l4d2panel:bootstrap-owner',0))`); err != nil {
		return domain.User{}, false, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return domain.User{}, false, err
	}
	if count > 0 {
		return domain.User{}, false, tx.Commit(ctx)
	}
	user := domain.User{ID: uuid.NewString(), Username: strings.TrimSpace(username), DisplayName: strings.TrimSpace(displayName), Role: domain.RoleOwner, CreatedAt: time.Now().UTC()}
	if user.Username == "" {
		user.Username = "admin"
	}
	if user.DisplayName == "" {
		user.DisplayName = user.Username
	}
	_, err = tx.Exec(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,is_bootstrap_owner,created_at)
		VALUES($1,$2,$3,$4,$5,$6,true,$7)`, user.ID, user.Username, normalizeUsername(user.Username), user.DisplayName, passwordHash, user.Role, user.CreatedAt)
	if err != nil {
		return domain.User{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO role_bindings(id,user_id,role,scope_type) VALUES($1,$2,'owner','global')`, uuid.NewString(), user.ID)
	if err != nil {
		return domain.User{}, false, err
	}
	return user, true, tx.Commit(ctx)
}

func scanAuthUser(row pgx.Row) (userAuthRecord, error) {
	var r userAuthRecord
	err := row.Scan(&r.User.ID, &r.User.Username, &r.User.DisplayName, &r.User.Role, &r.User.TOTPEnabled,
		&r.User.CreatedAt, &r.User.LastLoginAt, &r.PasswordHash, &r.TOTPSecret, &r.Disabled, &r.User.BootstrapOwner)
	r.User.Disabled = r.Disabled
	return r, err
}

func (s *Store) UserForLogin(ctx context.Context, username string) (userAuthRecord, error) {
	r, err := scanAuthUser(s.pool.QueryRow(ctx, `SELECT id,username,display_name,role,totp_enabled,created_at,last_login_at,
		password_hash,COALESCE(totp_secret,''),disabled,is_bootstrap_owner FROM users WHERE username_normalized=$1`, normalizeUsername(username)))
	if errors.Is(err, pgx.ErrNoRows) {
		return userAuthRecord{}, ErrNotFound
	}
	return r, err
}

func (s *Store) CreateSession(ctx context.Context, userID string, tokenDigest, csrfDigest []byte, remoteIP, userAgent string, expiresAt time.Time) (string, error) {
	id := uuid.NewString()
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_hash,remote_ip,user_agent,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, id, userID, tokenDigest, csrfDigest, remoteIP, userAgent, expiresAt)
	if err != nil {
		return "", err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, userID)
	return id, nil
}

func (s *Store) Session(ctx context.Context, tokenDigest []byte) (Principal, error) {
	var p Principal
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.username,u.display_name,u.role,u.totp_enabled,u.created_at,u.last_login_at,u.is_bootstrap_owner,
		s.id,s.csrf_hash,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.disabled`, tokenDigest).Scan(
		&p.User.ID, &p.User.Username, &p.User.DisplayName, &p.User.Role, &p.User.TOTPEnabled,
		&p.User.CreatedAt, &p.User.LastLoginAt, &p.User.BootstrapOwner, &p.SessionID, &p.CSRFHash, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err == nil {
		err = s.loadPrincipalBindings(ctx, &p)
	}
	if err == nil {
		_, _ = s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at=now() WHERE id=$1 AND last_seen_at<now()-interval '5 minutes'`, p.SessionID)
	}
	return p, err
}

func (s *Store) ActiveSessionByID(ctx context.Context, sessionID string) (Principal, error) {
	var p Principal
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.username,u.display_name,u.role,u.totp_enabled,u.created_at,u.last_login_at,u.is_bootstrap_owner,
		s.id,s.csrf_hash,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.id=$1 AND s.expires_at>now() AND NOT u.disabled`, sessionID).Scan(
		&p.User.ID, &p.User.Username, &p.User.DisplayName, &p.User.Role, &p.User.TOTPEnabled,
		&p.User.CreatedAt, &p.User.LastLoginAt, &p.User.BootstrapOwner, &p.SessionID, &p.CSRFHash, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err == nil {
		err = s.loadPrincipalBindings(ctx, &p)
	}
	return p, err
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, sessionID)
	return err
}

func (s *Store) RotateSessionCSRF(ctx context.Context, sessionID string, digest []byte) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET csrf_hash=$2 WHERE id=$1`, sessionID, digest)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SaveTOTPSecret(ctx context.Context, userID, encrypted string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET totp_secret=$2,totp_enabled=false WHERE id=$1`, userID, encrypted)
	return err
}

func (s *Store) TOTPSecret(ctx context.Context, userID string) (string, bool, error) {
	var secret string
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(totp_secret,''),totp_enabled FROM users WHERE id=$1`, userID).Scan(&secret, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return secret, enabled, err
}

func (s *Store) EnableTOTP(ctx context.Context, userID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET totp_enabled=true WHERE id=$1 AND totp_secret IS NOT NULL`, userID)
	if err == nil && tag.RowsAffected() == 0 {
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE users SET totp_enabled=true WHERE id=$1 AND totp_secret IS NOT NULL`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM totp_recovery_codes WHERE user_id=$1`, userID); err != nil {
		return err
	}
	for _, digest := range codeDigests {
		if len(digest) == 0 {
			return errors.New("recovery code digest is empty")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO totp_recovery_codes(id,user_id,code_hash) VALUES($1,$2,$3)`,
			uuid.NewString(), userID, digest); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ConsumeTOTPRecoveryCode uses a single conditional UPDATE so concurrent
// login attempts can never successfully reuse the same recovery code.
func (s *Store) ConsumeTOTPRecoveryCode(ctx context.Context, userID string, digest []byte) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE totp_recovery_codes SET used_at=now()
		WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL`, userID, digest)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
