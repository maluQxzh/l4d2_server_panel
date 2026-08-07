package control

import (
	"fmt"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type Permission string

const (
	PermissionRead         Permission = "read"
	PermissionOperate      Permission = "operate"
	PermissionManage       Permission = "manage"
	PermissionRootSecurity Permission = "root_security"
)

// instanceJobPermissions is the internal permission table for job types that
// can be persisted and dispatched to an Agent. It is deliberately separate
// from the public generic action endpoint: internal transfer, discovery, and
// provisioning jobs must not become public merely because they are queueable.
var instanceJobPermissions = map[string]Permission{
	"a2s.info":            PermissionOperate,
	"a2s.players":         PermissionOperate,
	"backup":              PermissionOperate,
	"backup.chunk":        PermissionRead,
	"backup.upload.abort": PermissionOperate,
	"ban_player":          PermissionOperate,
	"change_map":          PermissionOperate,
	"configure":           PermissionOperate,
	"create":              PermissionManage,
	"delete":              PermissionManage,
	"detach":              PermissionManage,
	"file":                PermissionOperate,
	"game.import":         PermissionManage,
	"import":              PermissionManage,
	"install":             PermissionManage,
	"install_mod":         PermissionManage,
	"qq_flash_import":     PermissionManage,
	"kick_player":         PermissionOperate,
	"list":                PermissionOperate,
	"map.assign":          PermissionManage,
	"map.unassign":        PermissionManage,
	"logs.chunk":          PermissionRead,
	"logs.search":         PermissionRead,
	"rcon":                PermissionOperate,
	"rcon.players":        PermissionOperate,
	"reinstall":           PermissionManage,
	"rollback_package":    PermissionManage,
	"restart":             PermissionOperate,
	"restore":             PermissionOperate,
	"start":               PermissionOperate,
	"stop":                PermissionOperate,
	"toggle_package":      PermissionManage,
	"unban_player":        PermissionOperate,
	"update":              PermissionOperate,
	"validate":            PermissionOperate,
}

// genericInstanceActionPermissions is the public contract for
// POST /instances/{instanceID}/actions. Some entries are API-level aliases
// which handlers normalize to an internal job type before queueing.
var genericInstanceActionPermissions = map[string]Permission{
	"backup":          PermissionOperate,
	"ban_player":      PermissionOperate,
	"broadcast":       PermissionOperate,
	"change_map":      PermissionOperate,
	"create_schedule": PermissionOperate,
	"install_package": PermissionManage,
	"kick_player":     PermissionOperate,
	"rcon":            PermissionOperate,
	"restart":         PermissionOperate,
	"restore":         PermissionOperate,
	"start":           PermissionOperate,
	"stop":            PermissionOperate,
	"toggle_package":  PermissionManage,
	"unban_player":    PermissionOperate,
}

func genericInstanceActionAllowed(action string) bool {
	_, ok := genericInstanceActionPermission(action)
	return ok
}

func genericInstanceActionPermission(action string) (Permission, bool) {
	permission, ok := genericInstanceActionPermissions[action]
	return permission, ok
}

func scheduledActionAllowed(action string) bool {
	switch action {
	case "backup", "broadcast", "restart", "start", "stop":
		return true
	default:
		return false
	}
}

func validateScheduledPayload(action string, payload map[string]any) error {
	allowed := map[string]bool{}
	switch action {
	case "start":
		allowed["health_timeout_seconds"] = true
	case "stop":
		allowed["timeout_seconds"], allowed["maintenance"], allowed["warning_seconds"] = true, true, true
	case "restart":
		allowed["timeout_seconds"], allowed["health_timeout_seconds"] = true, true
	case "update", "validate":
		allowed["timeout_seconds"], allowed["maintenance"], allowed["warning_seconds"], allowed["health_timeout_seconds"] = true, true, true, true
	case "backup":
		allowed["kind"], allowed["retention_daily"], allowed["retention_weekly"] = true, true, true
	case "broadcast":
		allowed["message"] = true
	default:
		return fmt.Errorf("action %q is not permitted in a persistent schedule", action)
	}
	for key := range payload {
		if !allowed[key] {
			return fmt.Errorf("field %q is not permitted in a persistent %s schedule", key, action)
		}
	}
	for _, key := range []string{"timeout_seconds", "health_timeout_seconds"} {
		if value, ok := payload[key]; ok {
			seconds, valid := scheduledInteger(value)
			if !valid || seconds < 1 || seconds > 300 {
				return fmt.Errorf("%s must be an integer between 1 and 300", key)
			}
		}
	}
	if value, ok := payload["maintenance"]; ok {
		if _, valid := value.(bool); !valid {
			return fmt.Errorf("maintenance must be a boolean")
		}
	}
	if value, ok := payload["warning_seconds"]; ok {
		items, valid := value.([]any)
		if !valid {
			if typed, typedOK := value.([]int); typedOK {
				items = make([]any, len(typed))
				for index, item := range typed {
					items[index] = item
				}
				valid = true
			}
		}
		if !valid || len(items) == 0 || len(items) > 10 {
			return fmt.Errorf("warning_seconds must contain between 1 and 10 integers")
		}
		for _, item := range items {
			seconds, itemValid := scheduledInteger(item)
			if !itemValid || seconds < 1 || seconds > 3600 {
				return fmt.Errorf("warning_seconds values must be integers between 1 and 3600")
			}
		}
	}
	if action == "backup" {
		if kind, ok := payload["kind"]; ok {
			value, valid := kind.(string)
			if !valid || (value != "daily" && value != "weekly") {
				return fmt.Errorf("scheduled backup kind must be daily or weekly")
			}
		}
		for key, maximum := range map[string]int{"retention_daily": 365, "retention_weekly": 52} {
			if value, ok := payload[key]; ok {
				count, valid := scheduledInteger(value)
				if !valid || count < 1 || count > maximum {
					return fmt.Errorf("%s must be an integer between 1 and %d", key, maximum)
				}
			}
		}
	}
	if action == "broadcast" {
		message, valid := payload["message"].(string)
		message = strings.TrimSpace(message)
		if !valid || message == "" || len(message) > 256 || strings.ContainsAny(message, "\x00\r\n;") {
			return fmt.Errorf("scheduled broadcast message is invalid")
		}
	}
	return nil
}

func scheduledInteger(value any) (int, bool) {
	switch item := value.(type) {
	case int:
		return item, true
	case int32:
		return int(item), true
	case int64:
		return int(item), true
	case float64:
		converted := int(item)
		return converted, item == float64(converted)
	default:
		return 0, false
	}
}

func InstanceActionPermission(action string) (Permission, bool) {
	permission, ok := instanceJobPermissions[action]
	return permission, ok
}

func RoleAllowsInstanceAction(role domain.Role, action string) bool {
	permission, ok := genericInstanceActionPermission(action)
	return ok && RoleAllows(role, permission)
}

func RoleAllows(role domain.Role, permission Permission) bool {
	switch permission {
	case PermissionRead:
		return role == domain.RoleViewer || role == domain.RoleOperator || role == domain.RoleAdmin || role == domain.RoleOwner
	case PermissionOperate:
		return role == domain.RoleOperator || role == domain.RoleAdmin || role == domain.RoleOwner
	case PermissionManage:
		return role == domain.RoleAdmin || role == domain.RoleOwner
	case PermissionRootSecurity:
		return role == domain.RoleOwner
	default:
		return false
	}
}
