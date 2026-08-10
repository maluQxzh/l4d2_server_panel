package control

import (
	"fmt"
	"sort"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type Permission string

const (
	PermissionRead         Permission = "read"
	PermissionOperate      Permission = "operate"
	PermissionManage       Permission = "manage"
	PermissionRootSecurity Permission = "root_security"

	PermissionNodesRead              Permission = "nodes.read"
	PermissionNodesEnroll            Permission = "nodes.enroll"
	PermissionNodesUpdate            Permission = "nodes.update"
	PermissionNodesCredentialsRotate Permission = "nodes.credentials.rotate"
	PermissionNodesDiscover          Permission = "nodes.discover"
	PermissionNodesQuarantineRead    Permission = "nodes.quarantine.read"
	PermissionNodesQuarantinePurge   Permission = "nodes.quarantine.purge"
	PermissionNodesDelete            Permission = "nodes.delete"
	PermissionEnvironmentsRead       Permission = "environments.read"
	PermissionEnvironmentsManage     Permission = "environments.manage"
	PermissionInstancesRead          Permission = "instances.read"
	PermissionInstancesCreate        Permission = "instances.create"
	PermissionInstancesConfigure     Permission = "instances.configure"
	PermissionInstancesDelete        Permission = "instances.delete"
	PermissionInstancesStart         Permission = "instances.start"
	PermissionInstancesStop          Permission = "instances.stop"
	PermissionInstancesRestart       Permission = "instances.restart"
	PermissionPlayersRead            Permission = "players.read"
	PermissionPlayersKick            Permission = "players.kick"
	PermissionPlayersBan             Permission = "players.ban"
	PermissionPlayersUnban           Permission = "players.unban"
	PermissionPlayersChangeMap       Permission = "players.change_map"
	PermissionPlayersBroadcast       Permission = "players.broadcast"
	PermissionRCONExecute            Permission = "rcon.execute"
	PermissionFilesList              Permission = "files.list"
	PermissionFilesRead              Permission = "files.read"
	PermissionFilesWrite             Permission = "files.write"
	PermissionFilesDelete            Permission = "files.delete"
	PermissionPackagesRead           Permission = "packages.read"
	PermissionPackagesInstall        Permission = "packages.install"
	PermissionPackagesToggle         Permission = "packages.toggle"
	PermissionPackagesDelete         Permission = "packages.delete"
	PermissionPackagesRollback       Permission = "packages.rollback"
	PermissionBackupsRead            Permission = "backups.read"
	PermissionBackupsDownload        Permission = "backups.download"
	PermissionBackupsCreate          Permission = "backups.create"
	PermissionBackupsRestore         Permission = "backups.restore"
	PermissionSchedulesRead          Permission = "schedules.read"
	PermissionSchedulesManage        Permission = "schedules.manage"
	PermissionLogsRead               Permission = "logs.read"
	PermissionMapsRead               Permission = "maps.read"
	PermissionMapsImport             Permission = "maps.import"
	PermissionMapsAssign             Permission = "maps.assign"
	PermissionMapsVerify             Permission = "maps.verify"
	PermissionMapsDelete             Permission = "maps.delete"
	PermissionJobsRead               Permission = "jobs.read"
	PermissionJobsDetails            Permission = "jobs.details"
	PermissionJobsCancel             Permission = "jobs.cancel"
	PermissionAuditRead              Permission = "audit.read"
	PermissionAuditReadGlobal        Permission = "audit.read_global"
	PermissionNotificationsRead      Permission = "notifications.read"
	PermissionNotificationsManage    Permission = "notifications.manage"
	PermissionMetricsRead            Permission = "metrics.read"
)

type permissionScope string

const (
	permissionScopeGlobal   permissionScope = "global"
	permissionScopeNode     permissionScope = "node"
	permissionScopeInstance permissionScope = "instance"
	permissionScopeResource permissionScope = "resource"
)

type permissionDefinition struct {
	Class Permission
	Scope permissionScope
}

var permissionDefinitions = map[Permission]permissionDefinition{
	PermissionNodesRead:              {PermissionRead, permissionScopeNode},
	PermissionNodesEnroll:            {PermissionManage, permissionScopeGlobal},
	PermissionNodesUpdate:            {PermissionManage, permissionScopeNode},
	PermissionNodesCredentialsRotate: {PermissionManage, permissionScopeNode},
	PermissionNodesDiscover:          {PermissionManage, permissionScopeNode},
	PermissionNodesQuarantineRead:    {PermissionManage, permissionScopeNode},
	PermissionNodesQuarantinePurge:   {PermissionManage, permissionScopeNode},
	PermissionNodesDelete:            {PermissionManage, permissionScopeNode},
	PermissionEnvironmentsRead:       {PermissionRead, permissionScopeNode},
	PermissionEnvironmentsManage:     {PermissionManage, permissionScopeNode},
	PermissionInstancesRead:          {PermissionRead, permissionScopeInstance},
	PermissionInstancesCreate:        {PermissionManage, permissionScopeNode},
	PermissionInstancesConfigure:     {PermissionOperate, permissionScopeInstance},
	PermissionInstancesDelete:        {PermissionManage, permissionScopeInstance},
	PermissionInstancesStart:         {PermissionOperate, permissionScopeInstance},
	PermissionInstancesStop:          {PermissionOperate, permissionScopeInstance},
	PermissionInstancesRestart:       {PermissionOperate, permissionScopeInstance},
	PermissionPlayersRead:            {PermissionRead, permissionScopeInstance},
	PermissionPlayersKick:            {PermissionOperate, permissionScopeInstance},
	PermissionPlayersBan:             {PermissionOperate, permissionScopeInstance},
	PermissionPlayersUnban:           {PermissionOperate, permissionScopeInstance},
	PermissionPlayersChangeMap:       {PermissionOperate, permissionScopeInstance},
	PermissionPlayersBroadcast:       {PermissionOperate, permissionScopeInstance},
	PermissionRCONExecute:            {PermissionOperate, permissionScopeInstance},
	PermissionFilesList:              {PermissionRead, permissionScopeInstance},
	PermissionFilesRead:              {PermissionOperate, permissionScopeInstance},
	PermissionFilesWrite:             {PermissionOperate, permissionScopeInstance},
	PermissionFilesDelete:            {PermissionOperate, permissionScopeInstance},
	PermissionPackagesRead:           {PermissionRead, permissionScopeInstance},
	PermissionPackagesInstall:        {PermissionManage, permissionScopeInstance},
	PermissionPackagesToggle:         {PermissionManage, permissionScopeInstance},
	PermissionPackagesDelete:         {PermissionManage, permissionScopeInstance},
	PermissionPackagesRollback:       {PermissionManage, permissionScopeInstance},
	PermissionBackupsRead:            {PermissionRead, permissionScopeInstance},
	PermissionBackupsDownload:        {PermissionOperate, permissionScopeInstance},
	PermissionBackupsCreate:          {PermissionOperate, permissionScopeInstance},
	PermissionBackupsRestore:         {PermissionOperate, permissionScopeInstance},
	PermissionSchedulesRead:          {PermissionRead, permissionScopeInstance},
	PermissionSchedulesManage:        {PermissionOperate, permissionScopeInstance},
	PermissionLogsRead:               {PermissionRead, permissionScopeInstance},
	PermissionMapsRead:               {PermissionRead, permissionScopeResource},
	PermissionMapsImport:             {PermissionManage, permissionScopeResource},
	PermissionMapsAssign:             {PermissionManage, permissionScopeInstance},
	PermissionMapsVerify:             {PermissionManage, permissionScopeResource},
	PermissionMapsDelete:             {PermissionManage, permissionScopeResource},
	PermissionJobsRead:               {PermissionRead, permissionScopeResource},
	PermissionJobsDetails:            {PermissionOperate, permissionScopeResource},
	PermissionJobsCancel:             {PermissionOperate, permissionScopeResource},
	PermissionAuditRead:              {PermissionRead, permissionScopeResource},
	PermissionAuditReadGlobal:        {PermissionManage, permissionScopeGlobal},
	PermissionNotificationsRead:      {PermissionRead, permissionScopeResource},
	PermissionNotificationsManage:    {PermissionManage, permissionScopeGlobal},
	PermissionMetricsRead:            {PermissionRead, permissionScopeResource},
	PermissionRootSecurity:           {PermissionRootSecurity, permissionScopeGlobal},
}

func permissionClass(permission Permission) (Permission, bool) {
	if permission == PermissionRead || permission == PermissionOperate || permission == PermissionManage || permission == PermissionRootSecurity {
		return permission, true
	}
	definition, ok := permissionDefinitions[permission]
	return definition.Class, ok
}

var operatorPresetExcludedPermissions = map[Permission]struct{}{
	PermissionPlayersKick:      {},
	PermissionPlayersBan:       {},
	PermissionPlayersUnban:     {},
	PermissionPlayersBroadcast: {},
	PermissionRCONExecute:      {},
	PermissionFilesRead:        {},
	PermissionFilesWrite:       {},
	PermissionFilesDelete:      {},
	PermissionBackupsDownload:  {},
	PermissionBackupsCreate:    {},
	PermissionBackupsRestore:   {},
	PermissionSchedulesManage:  {},
}

var operatorPresetAdditionalPermissions = map[Permission]struct{}{
	PermissionMapsRead:   {},
	PermissionMapsImport: {},
	PermissionMapsAssign: {},
	PermissionMapsVerify: {},
	PermissionMapsDelete: {},
}

func presetPermissions(role domain.Role) []string {
	items := make([]string, 0, len(permissionDefinitions))
	for permission := range permissionDefinitions {
		if RoleAllows(role, permission) {
			items = append(items, string(permission))
		}
	}
	sort.Strings(items)
	return items
}

func presetPermissionsForScope(role domain.Role, scopeType string) []string {
	items := presetPermissions(role)
	filtered := items[:0]
	for _, raw := range items {
		definition := permissionDefinitions[Permission(raw)]
		if scopeType == "global" || (scopeType == "node" && definition.Scope != permissionScopeGlobal) ||
			(scopeType == "instance" && (definition.Scope == permissionScopeInstance || definition.Scope == permissionScopeResource)) {
			filtered = append(filtered, raw)
		}
	}
	return filtered
}

func bindingAllows(binding domain.RoleBinding, permission Permission) bool {
	requestedClass, requestedIsClass := permissionClass(permission)
	requestedIsClass = requestedIsClass && requestedClass == permission && permission != PermissionRootSecurity
	for _, granted := range binding.Permissions {
		if Permission(granted) == permission {
			return true
		}
		if requestedIsClass {
			grantedClass, ok := permissionClass(Permission(granted))
			if ok && roleRankForPermissionClass(grantedClass) >= roleRankForPermissionClass(requestedClass) {
				return true
			}
		}
	}
	return false
}

func roleRankForPermissionClass(permission Permission) int {
	switch permission {
	case PermissionRootSecurity:
		return 4
	case PermissionManage:
		return 3
	case PermissionOperate:
		return 2
	case PermissionRead:
		return 1
	default:
		return 0
	}
}

// instanceJobPermissions is the internal permission table for job types that
// can be persisted and dispatched to an Agent. It is deliberately separate
// from the public generic action endpoint: internal transfer, discovery, and
// provisioning jobs must not become public merely because they are queueable.
var instanceJobPermissions = map[string]Permission{
	"a2s.info":               PermissionInstancesRead,
	"a2s.players":            PermissionPlayersRead,
	"backup":                 PermissionBackupsCreate,
	"backup.chunk":           PermissionBackupsDownload,
	"backup.upload.abort":    PermissionBackupsRestore,
	"ban_player":             PermissionPlayersBan,
	"change_map":             PermissionPlayersChangeMap,
	"configure":              PermissionInstancesConfigure,
	"create":                 PermissionInstancesCreate,
	"delete":                 PermissionInstancesDelete,
	"detach":                 PermissionInstancesDelete,
	"file":                   PermissionFilesWrite,
	"game.import":            PermissionInstancesCreate,
	"import":                 PermissionInstancesCreate,
	"install":                PermissionEnvironmentsManage,
	"install_mod":            PermissionPackagesInstall,
	"inspect_plugin_archive": PermissionPackagesInstall,
	"qq_flash_import":        PermissionPackagesInstall,
	"scan_packages":          PermissionPackagesInstall,
	"kick_player":            PermissionPlayersKick,
	"list":                   PermissionFilesList,
	"map.assign":             PermissionMapsAssign,
	"map.unassign":           PermissionMapsAssign,
	"logs.chunk":             PermissionLogsRead,
	"logs.search":            PermissionLogsRead,
	"rcon":                   PermissionRCONExecute,
	"rcon.players":           PermissionPlayersRead,
	"rollback_package":       PermissionPackagesRollback,
	"restart":                PermissionInstancesRestart,
	"restore":                PermissionBackupsRestore,
	"start":                  PermissionInstancesStart,
	"stop":                   PermissionInstancesStop,
	"toggle_package":         PermissionPackagesToggle,
	"delete_package":         PermissionPackagesDelete,
	"unban_player":           PermissionPlayersUnban,
	// Legacy instance maintenance jobs remain valid for durable job replay and
	// state projection, but maintenance authority now belongs to the selected
	// game installation's node-scoped runtime environment.
	"update":   PermissionEnvironmentsManage,
	"validate": PermissionEnvironmentsManage,
}

// genericInstanceActionPermissions is the public contract for
// POST /instances/{instanceID}/actions. Some entries are API-level aliases
// which handlers normalize to an internal job type before queueing.
var genericInstanceActionPermissions = map[string]Permission{
	"backup":          PermissionBackupsCreate,
	"ban_player":      PermissionPlayersBan,
	"broadcast":       PermissionPlayersBroadcast,
	"change_map":      PermissionPlayersChangeMap,
	"create_schedule": PermissionSchedulesManage,
	"install_package": PermissionPackagesInstall,
	"kick_player":     PermissionPlayersKick,
	"rcon":            PermissionRCONExecute,
	"restart":         PermissionInstancesRestart,
	"restore":         PermissionBackupsRestore,
	"start":           PermissionInstancesStart,
	"stop":            PermissionInstancesStop,
	"toggle_package":  PermissionPackagesToggle,
	"unban_player":    PermissionPlayersUnban,
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

// normalizedInstanceActionPermission keeps authorization tied to the public
// action after it is converted into a fixed internal Agent job. Broadcast is
// intentionally dispatched as a sanitized RCON command without granting the
// caller authority to submit arbitrary RCON commands.
func normalizedInstanceActionPermission(publicAction, internalAction string) (Permission, bool) {
	if publicAction == "broadcast" && internalAction == "rcon" {
		return PermissionPlayersBroadcast, true
	}
	return InstanceActionPermission(internalAction)
}

func RoleAllowsInstanceAction(role domain.Role, action string) bool {
	permission, ok := genericInstanceActionPermission(action)
	return ok && RoleAllows(role, permission)
}

func RoleAllows(role domain.Role, permission Permission) bool {
	if role == domain.RoleOperator {
		if _, excluded := operatorPresetExcludedPermissions[permission]; excluded {
			return false
		}
		if _, additional := operatorPresetAdditionalPermissions[permission]; additional {
			return true
		}
	}
	class, ok := permissionClass(permission)
	if !ok {
		return false
	}
	switch class {
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
