package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const roleBindingColumns = `id,user_id,role,scope_type,COALESCE(node_id,''),
	COALESCE(instance_id,''),COALESCE(created_by,''),created_at`

var errRootSecurityRequired = errors.New("global owner permission is required")

func requireRootSecurityActor(ctx context.Context, tx *sql.Tx, actorID string) error {
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM users u JOIN role_bindings rb ON rb.user_id=u.id
		WHERE u.id=?1 AND NOT u.disabled AND rb.scope_type='global' AND rb.role='owner'
	)`, actorID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return errRootSecurityRequired
	}
	return nil
}

func scanRoleBinding(row scanner) (domain.RoleBinding, error) {
	var binding domain.RoleBinding
	err := row.Scan(&binding.ID, &binding.UserID, &binding.Role, &binding.ScopeType, &binding.NodeID,
		&binding.InstanceID, &binding.CreatedBy, &binding.CreatedAt)
	return binding, err
}

func (s *Store) RoleBindings(ctx context.Context, userID string) ([]domain.RoleBinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+roleBindingColumns+` FROM role_bindings WHERE user_id=?1
		ORDER BY CASE scope_type WHEN 'global' THEN 0 WHEN 'node' THEN 1 ELSE 2 END,created_at,id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.RoleBinding, 0)
	for rows.Next() {
		item, err := scanRoleBinding(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	permissions, err := s.roleBindingPermissions(ctx, userID)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index].Permissions = permissions[items[index].ID]
	}
	return items, nil
}

func (s *Store) roleBindingPermissions(ctx context.Context, userID string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.binding_id,p.permission FROM role_binding_permissions p
		JOIN role_bindings rb ON rb.id=p.binding_id WHERE rb.user_id=?1 ORDER BY p.binding_id,p.permission`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]string)
	for rows.Next() {
		var bindingID, permission string
		if err := rows.Scan(&bindingID, &permission); err != nil {
			return nil, err
		}
		result[bindingID] = append(result[bindingID], permission)
	}
	return result, rows.Err()
}

func (s *Store) loadPrincipalBindings(ctx context.Context, principal *Principal) error {
	bindings, err := s.RoleBindings(ctx, principal.User.ID)
	if err != nil {
		return err
	}
	principal.Bindings = bindings
	principal.BindingsLoaded = true
	principal.User.RoleBindings = bindings
	return nil
}

func validRole(role domain.Role) bool {
	return role == domain.RoleOwner || role == domain.RoleAdmin || role == domain.RoleOperator || role == domain.RoleViewer
}

func roleRank(role domain.Role) int {
	switch role {
	case domain.RoleOwner:
		return 4
	case domain.RoleAdmin:
		return 3
	case domain.RoleOperator:
		return 2
	case domain.RoleViewer:
		return 1
	default:
		return 0
	}
}

func highestBindingRole(bindings []domain.RoleBinding) domain.Role {
	role := domain.RoleViewer
	for _, binding := range bindings {
		if roleRank(binding.Role) > roleRank(role) {
			role = binding.Role
		}
	}
	return role
}

func normalizeRoleBindings(bindings []domain.RoleBinding) ([]domain.RoleBinding, error) {
	if len(bindings) == 0 || len(bindings) > 500 {
		return nil, errors.New("role_bindings must contain between 1 and 500 bindings")
	}
	result := make([]domain.RoleBinding, 0, len(bindings))
	seen := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		binding.ID = ""
		binding.UserID = ""
		binding.CreatedBy = ""
		binding.CreatedAt = time.Time{}
		binding.ScopeType = strings.ToLower(strings.TrimSpace(binding.ScopeType))
		binding.NodeID = strings.TrimSpace(binding.NodeID)
		binding.InstanceID = strings.TrimSpace(binding.InstanceID)
		if !validRole(binding.Role) {
			return nil, fmt.Errorf("invalid role %q", binding.Role)
		}
		if len(binding.Permissions) == 0 {
			binding.Permissions = presetPermissionsForScope(binding.Role, binding.ScopeType)
		}
		if len(binding.Permissions) == 0 || len(binding.Permissions) > len(permissionDefinitions) {
			return nil, errors.New("permissions must contain at least one known permission")
		}
		permissionSeen := make(map[string]bool, len(binding.Permissions))
		permissions := make([]string, 0, len(binding.Permissions))
		for _, raw := range binding.Permissions {
			permission := Permission(strings.TrimSpace(raw))
			definition, ok := permissionDefinitions[permission]
			if !ok {
				return nil, fmt.Errorf("invalid permission %q", raw)
			}
			if permissionSeen[string(permission)] {
				continue
			}
			permissionSeen[string(permission)] = true
			permissions = append(permissions, string(permission))
			_ = definition
		}
		sort.Strings(permissions)
		binding.Permissions = permissions
		if binding.Role == domain.RoleOwner && binding.ScopeType != "global" {
			return nil, errors.New("owner role is only valid at global scope")
		}
		var key string
		switch binding.ScopeType {
		case "global":
			if binding.NodeID != "" || binding.InstanceID != "" {
				return nil, errors.New("global binding must not contain node_id or instance_id")
			}
			key = "global"
		case "node":
			if _, err := uuid.Parse(binding.NodeID); err != nil || binding.InstanceID != "" {
				return nil, errors.New("node binding requires a valid node_id only")
			}
			key = "node:" + binding.NodeID
		case "instance":
			if _, err := uuid.Parse(binding.InstanceID); err != nil || binding.NodeID != "" {
				return nil, errors.New("instance binding requires a valid instance_id only")
			}
			key = "instance:" + binding.InstanceID
		default:
			return nil, errors.New("scope_type must be global, node, or instance")
		}
		for _, raw := range binding.Permissions {
			permission := Permission(raw)
			definition := permissionDefinitions[permission]
			if permission == PermissionRootSecurity && (binding.Role != domain.RoleOwner || binding.ScopeType != "global") {
				return nil, errors.New("root_security is fixed to the global owner preset")
			}
			if binding.ScopeType == "node" && definition.Scope == permissionScopeGlobal {
				return nil, fmt.Errorf("permission %q is only valid at global scope", permission)
			}
			if binding.ScopeType == "instance" && definition.Scope != permissionScopeInstance && definition.Scope != permissionScopeResource {
				return nil, fmt.Errorf("permission %q is not valid at instance scope", permission)
			}
		}
		if (permissionSetNeeds(binding.Permissions, permissionScopeNode) || (binding.ScopeType == "node" && permissionSetHasNonRead(binding.Permissions))) && !permissionSeen[string(PermissionNodesRead)] {
			return nil, errors.New("node operation permissions require nodes.read")
		}
		if (permissionSetNeeds(binding.Permissions, permissionScopeInstance) || (binding.ScopeType == "instance" && permissionSetHasNonRead(binding.Permissions))) && !permissionSeen[string(PermissionInstancesRead)] {
			return nil, errors.New("instance operation permissions require instances.read")
		}
		if binding.Role == domain.RoleOwner && !equalStringSets(binding.Permissions, presetPermissionsForScope(domain.RoleOwner, "global")) {
			return nil, errors.New("owner permissions cannot be customized")
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate role binding for %s", key)
		}
		seen[key] = true
		result = append(result, binding)
	}
	return result, nil
}

func permissionSetNeeds(permissions []string, scope permissionScope) bool {
	for _, raw := range permissions {
		definition := permissionDefinitions[Permission(raw)]
		if definition.Scope == scope && definition.Class != PermissionRead {
			return true
		}
	}
	return false
}

func permissionSetHasNonRead(permissions []string) bool {
	for _, raw := range permissions {
		if permissionDefinitions[Permission(raw)].Class != PermissionRead {
			return true
		}
	}
	return false
}

func validateBindingResources(ctx context.Context, tx *sql.Tx, bindings []domain.RoleBinding) error {
	for _, binding := range bindings {
		var exists bool
		switch binding.ScopeType {
		case "node":
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=?1)`, binding.NodeID).Scan(&exists); err != nil {
				return err
			}
		case "instance":
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM instances WHERE id=?1)`, binding.InstanceID).Scan(&exists); err != nil {
				return err
			}
		default:
			continue
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func insertRoleBindings(ctx context.Context, tx *sql.Tx, userID, actorID string, bindings []domain.RoleBinding) error {
	for _, binding := range bindings {
		bindingID := uuid.NewString()
		_, err := tx.ExecContext(ctx, `INSERT INTO role_bindings(id,user_id,role,scope_type,node_id,instance_id,created_by)
			VALUES(?1,?2,?3,?4,NULLIF(?5,''),NULLIF(?6,''),NULLIF(?7,''))`,
			bindingID, userID, binding.Role, binding.ScopeType, binding.NodeID, binding.InstanceID, actorID)
		if err != nil {
			return err
		}
		for _, permission := range binding.Permissions {
			if _, err := tx.ExecContext(ctx, `INSERT INTO role_binding_permissions(binding_id,permission) VALUES(?1,?2)`, bindingID, permission); err != nil {
				return err
			}
		}
	}
	return nil
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type CreateTeamUserInput struct {
	Username     string
	DisplayName  string
	PasswordHash string
	Bindings     []domain.RoleBinding
}

func (s *Store) CreateTeamUser(ctx context.Context, actorID string, input CreateTeamUserInput) (domain.User, error) {
	bindings, err := normalizeRoleBindings(input.Bindings)
	if err != nil {
		return domain.User{}, err
	}
	input.Username = strings.TrimSpace(input.Username)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if input.Username == "" || len(input.Username) > 128 {
		return domain.User{}, errors.New("username must be between 1 and 128 bytes")
	}
	if input.DisplayName == "" {
		input.DisplayName = input.Username
	}
	if len(input.DisplayName) > 128 {
		return domain.User{}, errors.New("display_name must not exceed 128 bytes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback()
	if err := requireRootSecurityActor(ctx, tx, actorID); err != nil {
		return domain.User{}, err
	}
	if err := validateBindingResources(ctx, tx, bindings); err != nil {
		return domain.User{}, err
	}
	user := domain.User{ID: uuid.NewString(), Username: input.Username, DisplayName: input.DisplayName,
		Role: highestBindingRole(bindings), RoleBindings: bindings, CreatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7)`, user.ID, user.Username, normalizeUsername(user.Username), user.DisplayName,
		input.PasswordHash, user.Role, user.CreatedAt)
	if err != nil {
		if isSQLiteConstraint(err) {
			return domain.User{}, fmt.Errorf("username already exists: %w", ErrConflict)
		}
		return domain.User{}, err
	}
	if err := insertRoleBindings(ctx, tx, user.ID, actorID, bindings); err != nil {
		return domain.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.User{}, err
	}
	stored, err := s.TeamUser(ctx, user.ID)
	return stored, err
}

type UpdateTeamUserInput struct {
	DisplayName  *string
	PasswordHash *string
	Disabled     *bool
	Bindings     *[]domain.RoleBinding
}

func (s *Store) UpdateTeamUser(ctx context.Context, actorID, userID string, input UpdateTeamUserInput) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback()
	if err := requireRootSecurityActor(ctx, tx, actorID); err != nil {
		return domain.User{}, err
	}
	var bootstrap, disabled bool
	var currentRole domain.Role
	err = tx.QueryRowContext(ctx, `SELECT is_bootstrap_owner,disabled,role FROM users WHERE id=?1`, userID).
		Scan(&bootstrap, &disabled, &currentRole)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, err
	}
	bindingsChanged := input.Bindings != nil
	var bindings []domain.RoleBinding
	if input.Bindings != nil {
		bindings, err = normalizeRoleBindings(*input.Bindings)
		if err != nil {
			return domain.User{}, err
		}
		if err := validateBindingResources(ctx, tx, bindings); err != nil {
			return domain.User{}, err
		}
		if bootstrap && !containsGlobalOwner(bindings) {
			return domain.User{}, errors.New("bootstrap owner cannot be demoted")
		}
	}
	if input.Disabled != nil && *input.Disabled && bootstrap {
		return domain.User{}, errors.New("bootstrap owner cannot be disabled")
	}
	newDisabled := disabled
	if input.Disabled != nil {
		newDisabled = *input.Disabled
	}
	newRole := currentRole
	if input.Bindings != nil {
		newRole = highestBindingRole(bindings)
	}
	if (currentRole == domain.RoleOwner && (newRole != domain.RoleOwner || newDisabled)) || (input.Bindings != nil && !containsGlobalOwner(bindings)) {
		var otherOwners int
		if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT u.id) FROM users u JOIN role_bindings rb ON rb.user_id=u.id
			WHERE u.id<>?1 AND NOT u.disabled AND rb.scope_type='global' AND rb.role='owner'`, userID).Scan(&otherOwners); err != nil {
			return domain.User{}, err
		}
		if otherOwners == 0 && currentRole == domain.RoleOwner {
			return domain.User{}, errors.New("at least one enabled global owner is required")
		}
	}
	if input.DisplayName != nil {
		value := strings.TrimSpace(*input.DisplayName)
		if value == "" || len(value) > 128 {
			return domain.User{}, errors.New("display_name must be between 1 and 128 bytes")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name=?2 WHERE id=?1`, userID, value); err != nil {
			return domain.User{}, err
		}
	}
	if input.PasswordHash != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?2 WHERE id=?1`, userID, *input.PasswordHash); err != nil {
			return domain.User{}, err
		}
	}
	if input.Disabled != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled=?2 WHERE id=?1`, userID, *input.Disabled); err != nil {
			return domain.User{}, err
		}
	}
	if input.Bindings != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM role_bindings WHERE user_id=?1`, userID); err != nil {
			return domain.User{}, err
		}
		if err := insertRoleBindings(ctx, tx, userID, actorID, bindings); err != nil {
			return domain.User{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role=?2 WHERE id=?1`, userID, newRole); err != nil {
			return domain.User{}, err
		}
	}
	if input.PasswordHash != nil || (input.Disabled != nil && *input.Disabled) || bindingsChanged {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?1`, userID); err != nil {
			return domain.User{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=COALESCE(revoked_at,unixepoch('subsec')*1000000) WHERE user_id=?1`, userID); err != nil {
			return domain.User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.User{}, err
	}
	return s.TeamUser(ctx, userID)
}

func containsGlobalOwner(bindings []domain.RoleBinding) bool {
	for _, binding := range bindings {
		if binding.ScopeType == "global" && binding.Role == domain.RoleOwner {
			return true
		}
	}
	return false
}

func (s *Store) TeamUser(ctx context.Context, userID string) (domain.User, error) {
	var user domain.User
	err := s.db.QueryRowContext(ctx, `SELECT id,username,display_name,role,totp_enabled,created_at,last_login_at,disabled,is_bootstrap_owner
		FROM users WHERE id=?1`, userID).Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &user.TOTPEnabled,
		&user.CreatedAt, &user.LastLoginAt, &user.Disabled, &user.BootstrapOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, err
	}
	user.RoleBindings, err = s.RoleBindings(ctx, user.ID)
	return user, err
}

func (s *Store) ListTeamUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,display_name,role,totp_enabled,created_at,last_login_at,disabled,is_bootstrap_owner
		FROM users ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.User, 0)
	for rows.Next() {
		var item domain.User
		if err := rows.Scan(&item.ID, &item.Username, &item.DisplayName, &item.Role, &item.TOTPEnabled,
			&item.CreatedAt, &item.LastLoginAt, &item.Disabled, &item.BootstrapOwner); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range items {
		items[index].RoleBindings, err = s.RoleBindings(ctx, items[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}
