import type { InstanceRecord } from "./types";

export function instanceAddress(instance: Pick<InstanceRecord, "host" | "port" | "node_public_address">) {
  const host = (instance.node_public_address?.trim() || instance.host.trim()).replace(/^\[|\]$/g, "");
  if (!host || host === "0.0.0.0" || host === "::") return "未设置连接地址";
  return `${host.includes(":") ? `[${host}]` : host}:${instance.port}`;
}

export function instancePlayers(instance: Pick<InstanceRecord, "status" | "players">) {
  return ["running", "degraded", "external", "stopping"].includes(instance.status) ? instance.players : 0;
}
