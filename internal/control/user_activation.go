package control

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	userActivationCredentialPrefix = "l4d2p_once_"
	userActivationTTL              = 24 * time.Hour
	activationSessionTTL           = 10 * time.Minute
	pendingActivationPasswordHash  = "!pending-activation"
	standardSessionKind            = "standard"
	activationSessionKind          = "activation"
)

var ErrActivationRequired = errors.New("account activation is required")

type UserActivationCredential struct {
	OneTimePassword string    `json:"one_time_password"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func newUserActivationCredential() (UserActivationCredential, error) {
	random, err := randomToken(24)
	if err != nil {
		return UserActivationCredential{}, err
	}
	return UserActivationCredential{
		OneTimePassword: userActivationCredentialPrefix + random,
		ExpiresAt:       time.Now().UTC().Add(userActivationTTL),
	}, nil
}

type BeginUserActivationInput struct {
	UserID           string
	OneTimePassword  string
	SessionTokenHash []byte
	CSRFTokenHash    []byte
	RemoteIP         string
	UserAgent        string
	SessionExpiresAt time.Time
}

func (s *Store) BeginUserActivation(ctx context.Context, input BeginUserActivationInput) (bool, error) {
	raw := input.OneTimePassword
	if len(raw) > 128 || len(raw) <= len(userActivationCredentialPrefix) || raw[:len(userActivationCredentialPrefix)] != userActivationCredentialPrefix {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var stored []byte
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT a.token_hash,a.expires_at FROM user_activation_tokens a JOIN users u ON u.id=a.user_id
		WHERE a.user_id=?1 AND u.account_state='pending_activation' AND NOT u.disabled`, input.UserID).Scan(&stored, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	provided := tokenHash(raw)
	if !expiresAt.After(time.Now().UTC()) || len(stored) != len(provided) || subtle.ConstantTimeCompare(stored, provided) != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_activation_tokens WHERE user_id=?1`, input.UserID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?1`, input.UserID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_hash,remote_ip,user_agent,expires_at,kind)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8)`, uuid.NewString(), input.UserID, input.SessionTokenHash, input.CSRFTokenHash,
		input.RemoteIP, input.UserAgent, input.SessionExpiresAt, activationSessionKind); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ReissueUserActivation(ctx context.Context, actorID, userID string) (UserActivationCredential, error) {
	credential, err := newUserActivationCredential()
	if err != nil {
		return UserActivationCredential{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserActivationCredential{}, err
	}
	defer tx.Rollback()
	if err := requireRootSecurityActor(ctx, tx, actorID); err != nil {
		return UserActivationCredential{}, err
	}
	var state domain.AccountState
	if err := tx.QueryRowContext(ctx, `SELECT account_state FROM users WHERE id=?1`, userID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return UserActivationCredential{}, ErrNotFound
	} else if err != nil {
		return UserActivationCredential{}, err
	}
	if state != domain.AccountPendingActivation {
		return UserActivationCredential{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_activation_tokens(user_id,token_hash,created_by,expires_at)
		VALUES(?1,?2,?3,?4) ON CONFLICT(user_id) DO UPDATE SET token_hash=excluded.token_hash,created_by=excluded.created_by,
		expires_at=excluded.expires_at,created_at=unixepoch('subsec')*1000000`, userID, tokenHash(credential.OneTimePassword), actorID, credential.ExpiresAt); err != nil {
		return UserActivationCredential{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?1`, userID); err != nil {
		return UserActivationCredential{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=COALESCE(revoked_at,unixepoch('subsec')*1000000) WHERE user_id=?1`, userID); err != nil {
		return UserActivationCredential{}, err
	}
	return credential, tx.Commit()
}

type CompleteUserActivationInput struct {
	UserID            string
	ActivationSession string
	PasswordHash      string
	SessionTokenHash  []byte
	CSRFTokenHash     []byte
	RemoteIP          string
	UserAgent         string
	SessionExpiresAt  time.Time
}

func (s *Store) CompleteUserActivation(ctx context.Context, input CompleteUserActivationInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state domain.AccountState
	var sessionKind string
	err = tx.QueryRowContext(ctx, `SELECT u.account_state,s.kind FROM users u
		JOIN sessions s ON s.user_id=u.id
		WHERE u.id=?1 AND s.id=?2 AND NOT u.disabled AND s.expires_at>?3`, input.UserID, input.ActivationSession, time.Now().UTC()).
		Scan(&state, &sessionKind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if state != domain.AccountPendingActivation || sessionKind != activationSessionKind {
		return ErrActivationRequired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?2,account_state='active',last_login_at=unixepoch('subsec')*1000000 WHERE id=?1`, input.UserID, input.PasswordHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_activation_tokens WHERE user_id=?1`, input.UserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?1`, input.UserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=COALESCE(revoked_at,unixepoch('subsec')*1000000) WHERE user_id=?1`, input.UserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_hash,remote_ip,user_agent,expires_at,kind)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8)`, uuid.NewString(), input.UserID, input.SessionTokenHash, input.CSRFTokenHash,
		input.RemoteIP, input.UserAgent, input.SessionExpiresAt, standardSessionKind); err != nil {
		return err
	}
	return tx.Commit()
}
