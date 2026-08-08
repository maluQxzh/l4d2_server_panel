import type { InstanceConfig } from "./types";

export type RCONPasswordChange =
  | { kind: "unchanged" }
  | { kind: "set"; value: string }
  | { kind: "clear" };

export interface InstanceConfigUpdate {
  version: number;
  config: InstanceConfig;
  port?: number;
  rcon_password?: string;
}

/**
 * Keeps secrets outside the versioned public configuration object. An
 * untouched password is omitted, while an explicit clear is represented by
 * the empty top-level value expected by the control API.
 */
export function buildInstanceConfigUpdate(
  version: number,
  config: InstanceConfig,
  password: RCONPasswordChange,
  port?: number,
): InstanceConfigUpdate {
  // Do not preserve legacy connection-password or region fields when a modern
  // client writes the current configuration.
  const { server_password: _legacyPassword, region: _legacyRegion, ...safeConfig } = config as InstanceConfig & { server_password?: string };
  const update: InstanceConfigUpdate = { version, config: safeConfig };
  if (port !== undefined) update.port = port;
  if (password.kind === "set") update.rcon_password = password.value;
  if (password.kind === "clear") update.rcon_password = "";
  return update;
}
