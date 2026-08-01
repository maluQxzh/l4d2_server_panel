import type { InstanceConfig, InstanceRecord, InstanceStatus, Role, User } from "./types";

export type InstancePermission = "read" | "operate" | "manage";
export type LifecycleAction = "start" | "stop" | "restart" | "validate" | "update" | "reinstall" | "detach" | "quarantine";

function roleAllows(role: Role, permission: InstancePermission) {
  if (permission === "read") return true;
  if (permission === "operate") return role !== "viewer";
  return role === "owner" || role === "admin";
}

export function canAccessInstance(user: User | null, instance: Pick<InstanceRecord, "id" | "node_id">, permission: InstancePermission) {
  if (!user) return false;
  const bindings = user.role_bindings || [];
  if (!bindings.length) return roleAllows(user.role, permission);
  return bindings.some((binding) => {
    const matches = binding.scope_type === "global"
      || (binding.scope_type === "node" && binding.node_id === instance.node_id)
      || (binding.scope_type === "instance" && binding.instance_id === instance.id);
    return matches && roleAllows(binding.role, permission);
  });
}

export function canAccessNode(user: User | null, nodeID: string, permission: InstancePermission) {
  if (!user) return false;
  const bindings = user.role_bindings || [];
  if (!bindings.length) return roleAllows(user.role, permission);
  return bindings.some((binding) => {
    const matches = binding.scope_type === "global" || (binding.scope_type === "node" && binding.node_id === nodeID);
    return matches && roleAllows(binding.role, permission);
  });
}

const sensitiveLaunchArguments = new Set([
  "+rcon_password", "rcon_password", "+sv_password", "sv_password",
]);

export function sanitizeStartupExtraArguments(extraArgs: string[] = []) {
  const safe: string[] = [];
  for (let index = 0; index < extraArgs.length; index += 1) {
    const argument = String(extraArgs[index] || "");
    const sensitiveMatch = argument.trimStart().match(/^(\+?rcon_password|\+?sv_password)(?:\s|=|$)/i);
    const normalized = sensitiveMatch?.[1].toLowerCase() || "";
    if (!sensitiveMatch || !sensitiveLaunchArguments.has(normalized)) {
      safe.push(argument);
      continue;
    }
    safe.push(`${normalized} [已隐藏且不会传递]`);
    if (argument.toLowerCase() === normalized && index + 1 < extraArgs.length) index += 1;
  }
  return safe;
}

export function lifecycleActionAllowed(
  action: LifecycleAction,
  status: InstanceStatus,
  permissions: { operate: boolean; manage: boolean },
) {
  const stableManaged = status === "stopped" || status === "running" || status === "degraded";
  switch (action) {
  case "start":
    return permissions.operate && status === "stopped";
  case "stop":
    return permissions.operate && (status === "running" || status === "degraded");
  case "restart":
    return permissions.operate && (status === "running" || status === "degraded" || status === "external");
  case "validate":
  case "update":
    return permissions.operate && stableManaged;
  case "reinstall":
    return permissions.manage && stableManaged;
  case "detach":
    return permissions.manage && (status === "stopped" || status === "external");
  case "quarantine":
    return permissions.manage && status === "stopped";
  default:
    return false;
  }
}

export function buildStartupArgumentPreview(
  instance: Pick<InstanceRecord, "port" | "tv_port" | "config">,
  config: InstanceConfig = instance.config,
) {
  const args = ["-console", "-game", "left4dead2", "-port", String(instance.port)];
  if (instance.tv_port) args.push("+tv_port", String(instance.tv_port));
  args.push(
    "+map", config.start_map || "c1m1_hotel",
    "+mp_gamemode", config.mode || "coop",
    "+maxplayers", String(config.max_players || 8),
    "+exec", "l4d2panel.cfg",
  );
  return args.concat(sanitizeStartupExtraArguments(config.extra_args));
}

export function maintenancePayload(status: InstanceStatus) {
  return status === "running" || status === "degraded"
    ? { maintenance: true, warning_seconds: [600, 300, 60] }
    : undefined;
}
