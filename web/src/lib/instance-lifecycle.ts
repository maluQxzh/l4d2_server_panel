import { hasInstancePermission, hasNodePermission } from "./account-permissions";
import type { InstanceConfig, InstanceRecord, InstanceStatus, Permission, User } from "./types";

export type LifecycleAction = "start" | "stop" | "restart" | "validate" | "update" | "reinstall" | "detach" | "quarantine";

export function canAccessInstance(user: User | null, instance: Pick<InstanceRecord, "id" | "node_id">, permission: Permission) {
  return hasInstancePermission(user, instance, permission);
}

export function canAccessNode(user: User | null, nodeID: string, permission: Permission) {
  return hasNodePermission(user, nodeID, permission);
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
  allows: (permission: Permission) => boolean,
) {
  const stableManaged = status === "stopped" || status === "running" || status === "degraded";
  switch (action) {
  case "start":
    return allows("instances.start") && status === "stopped";
  case "stop":
    return allows("instances.stop") && (status === "running" || status === "degraded");
  case "restart":
    return allows("instances.restart") && (status === "running" || status === "degraded" || status === "external");
  case "validate":
    return allows("instances.validate") && stableManaged;
  case "update":
    return allows("instances.update") && stableManaged;
  case "reinstall":
    return allows("instances.delete") && stableManaged;
  case "detach":
    return allows("instances.delete") && (status === "stopped" || status === "external");
  case "quarantine":
    return allows("instances.delete") && status === "stopped";
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
  );
  if (config.difficulty) args.push("+z_difficulty", config.difficulty);
  return args.concat(sanitizeStartupExtraArguments(config.extra_args));
}
