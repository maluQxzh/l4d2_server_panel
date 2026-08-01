package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const apiTokenPrefix = "l4d2p_"

type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	NodeID     string     `json:"node_id,omitempty"`
	InstanceID string     `json:"instance_id,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func normalizeTokenScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > 3 {
		return nil, errors.New("scopes must contain read, operate, or manage")
	}
	seen := make(map[string]bool, len(scopes))
	result := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		scope := strings.ToLower(strings.TrimSpace(raw))
		switch scope {
		case string(PermissionRead), string(PermissionOperate), string(PermissionManage):
		default:
			return nil, fmt.Errorf("unsupported API token scope %q", raw)
		}
		if !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("at least one API token scope is required")
	}
	return result, nil
}

func tokenScopesAllow(scopes []string, permission Permission) bool {
	for _, scope := range scopes {
		switch Permission(scope) {
		case PermissionManage:
			if permission == PermissionManage || permission == PermissionOperate || permission == PermissionRead {
				return true
			}
		case PermissionOperate:
			if permission == PermissionOperate || permission == PermissionRead {
				return true
			}
		case PermissionRead:
			if permission == PermissionRead {
				return true
			}
		}
	}
	return false
}

func validateTokenScopesForRole(role domain.Role, scopes []string) error {
	for _, scope := range scopes {
		if !RoleAllows(role, Permission(scope)) {
			return fmt.Errorf("scope %q exceeds the user's role", scope)
		}
	}
	return nil
}

func newAPITokenSecret() (string, error) {
	secret, err := randomToken(32)
	if err != nil {
		return "", err
	}
	return apiTokenPrefix + secret, nil
}

const apiTokenColumns = `id,user_id,name,scope_mask,COALESCE(node_id,''),COALESCE(instance_id,''),
	expires_at,revoked_at,last_used_at,created_at`

func tokenScopeMask(scopes []string) int64 {
	var mask int64
	for _, scope := range scopes {
		switch Permission(scope) {
		case PermissionRead:
			mask |= 1
		case PermissionOperate:
			mask |= 2
		case PermissionManage:
			mask |= 4
		}
	}
	return mask
}

func tokenScopesFromMask(mask int64) []string {
	scopes := make([]string, 0, 3)
	if mask&1 != 0 {
		scopes = append(scopes, string(PermissionRead))
	}
	if mask&2 != 0 {
		scopes = append(scopes, string(PermissionOperate))
	}
	if mask&4 != 0 {
		scopes = append(scopes, string(PermissionManage))
	}
	return scopes
}

func scanAPIToken(row scanner) (APIToken, error) {
	var token APIToken
	var scopeMask int64
	err := row.Scan(&token.ID, &token.UserID, &token.Name, &scopeMask, &token.NodeID, &token.InstanceID,
		&token.ExpiresAt, &token.RevokedAt, &token.LastUsedAt, &token.CreatedAt)
	token.Scopes = tokenScopesFromMask(scopeMask)
	return token, err
}

func (s *Store) ValidateAPITokenResources(ctx context.Context, nodeID, instanceID string) error {
	if nodeID != "" {
		if _, err := uuid.Parse(nodeID); err != nil {
			return errors.New("node_id must be a UUID")
		}
		if _, err := s.GetNode(ctx, nodeID); err != nil {
			return fmt.Errorf("node_id: %w", err)
		}
	}
	if instanceID != "" {
		if _, err := uuid.Parse(instanceID); err != nil {
			return errors.New("instance_id must be a UUID")
		}
		instance, err := s.GetInstance(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("instance_id: %w", err)
		}
		if nodeID != "" && instance.NodeID != nodeID {
			return errors.New("instance_id does not belong to node_id")
		}
	}
	return nil
}

func (s *Store) CreateAPIToken(ctx context.Context, token APIToken, digest []byte) (APIToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIToken{}, err
	}
	defer tx.Rollback()
	// Bound active credentials per account and serialize concurrent creation.
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE user_id=?1 AND revoked_at IS NULL
		AND (expires_at IS NULL OR expires_at>?2)`, token.UserID, time.Now().UTC()).Scan(&active); err != nil {
		return APIToken{}, err
	}
	if active >= 100 {
		return APIToken{}, errors.New("active API token limit reached")
	}
	token.ID = uuid.NewString()
	token.CreatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask,node_id,instance_id,expires_at,created_at)
		VALUES(?1,?2,?3,?4,?5,NULLIF(?6,''),NULLIF(?7,''),?8,?9)`, token.ID, token.UserID, digest,
		token.Name, tokenScopeMask(token.Scopes), token.NodeID, token.InstanceID, token.ExpiresAt, token.CreatedAt)
	if err != nil {
		return APIToken{}, err
	}
	return token, tx.Commit()
}

func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE user_id=?1 ORDER BY created_at DESC,id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]APIToken, 0)
	for rows.Next() {
		token, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, token)
	}
	return items, rows.Err()
}

func (s *Store) RevokeAPIToken(ctx context.Context, userID, tokenID string) error {
	if _, err := uuid.Parse(tokenID); err != nil {
		return ErrNotFound
	}
	tag, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=COALESCE(revoked_at,unixepoch('subsec')*1000000) WHERE id=?1 AND user_id=?2`, tokenID, userID)
	if err == nil && resultRowsAffected(tag) == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) APITokenPrincipal(ctx context.Context, digest []byte) (Principal, error) {
	return s.apiTokenPrincipal(ctx, `t.token_hash=?1`, digest)
}

func (s *Store) ActiveAPITokenByID(ctx context.Context, tokenID string) (Principal, error) {
	return s.apiTokenPrincipal(ctx, `t.id=?1`, tokenID)
}

func (s *Store) apiTokenPrincipal(ctx context.Context, predicate string, argument any) (Principal, error) {
	var principal Principal
	var scopeMask int64
	now := time.Now().UTC()
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.display_name,u.role,u.totp_enabled,u.created_at,u.last_login_at,u.is_bootstrap_owner,
		t.id,t.scope_mask,COALESCE(t.node_id,''),COALESCE(t.instance_id,''),t.expires_at
		FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE `+predicate+`
		AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>?2) AND NOT u.disabled`, argument, now).Scan(
		&principal.User.ID, &principal.User.Username, &principal.User.DisplayName, &principal.User.Role,
		&principal.User.TOTPEnabled, &principal.User.CreatedAt, &principal.User.LastLoginAt, &principal.User.BootstrapOwner, &principal.APITokenID,
		&scopeMask, &principal.TokenNodeID, &principal.TokenInstanceID, &principal.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err == nil {
		principal.TokenScopes = tokenScopesFromMask(scopeMask)
		err = s.loadPrincipalBindings(ctx, &principal)
	}
	if err == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=?2 WHERE id=?1
			AND (last_used_at IS NULL OR last_used_at<?3)`, principal.APITokenID, now, now.Add(-5*time.Minute))
	}
	return principal, err
}

func (s *Store) RevalidatePrincipal(ctx context.Context, principal Principal) (Principal, error) {
	if principal.IsAPIToken() {
		return s.ActiveAPITokenByID(ctx, principal.APITokenID)
	}
	if principal.SessionID == "" {
		return Principal{}, ErrUnauthorized
	}
	return s.ActiveSessionByID(ctx, principal.SessionID)
}
