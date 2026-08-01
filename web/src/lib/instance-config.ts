import type { InstanceConfig } from "./types";

export type ServerPasswordChange =
  | { kind: "unchanged" }
  | { kind: "set"; value: string }
  | { kind: "clear" };

export interface InstanceConfigUpdate {
  version: number;
  config: InstanceConfig;
  server_password?: string;
}

/**
 * Keeps secrets outside the versioned public configuration object. An
 * untouched password is omitted, while an explicit clear is represented by
 * the empty top-level value expected by the control API.
 */
export function buildInstanceConfigUpdate(
  version: number,
  config: InstanceConfig,
  password: ServerPasswordChange,
): InstanceConfigUpdate {
  // Defensively discard values from older clients/state that used to put the
  // password in config. InstanceConfig intentionally no longer declares it.
  const { server_password: _legacyPassword, ...safeConfig } = config as InstanceConfig & { server_password?: string };
  const update: InstanceConfigUpdate = {
    version,
    config: {
      ...safeConfig,
      // Keep the field explicit so an empty input reliably disables an
      // existing automatic-update window instead of being treated as absent.
      maintenance_cron: safeConfig.maintenance_cron?.trim() || "",
    } as InstanceConfig,
  };
  if (password.kind === "set") update.server_password = password.value;
  if (password.kind === "clear") update.server_password = "";
  return update;
}
