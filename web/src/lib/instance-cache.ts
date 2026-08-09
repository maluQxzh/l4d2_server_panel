import type { QueryClient } from "@tanstack/react-query";
import type { InstanceRecord, InstanceStatus, ListResponse } from "./types";

export function setCachedInstance(client: QueryClient, instance: InstanceRecord) {
  client.setQueryData<InstanceRecord>(["instances", instance.id], instance);
  client.setQueryData<ListResponse<InstanceRecord>>(["instances"], (current) => {
    if (!current) return current;
    const index = current.items.findIndex((item) => item.id === instance.id);
    if (index < 0) return current;
    const items = [...current.items];
    items[index] = instance;
    return { ...current, items };
  });
}

export function setCachedInstanceStatus(client: QueryClient, instanceID: string, status: InstanceStatus) {
  const updatedAt = new Date().toISOString();
  client.setQueryData<InstanceRecord>(["instances", instanceID], (current) => current ? { ...current, status, updated_at: updatedAt } : current);
  client.setQueryData<ListResponse<InstanceRecord>>(["instances"], (current) => current ? {
    ...current,
    items: current.items.map((item) => item.id === instanceID ? { ...item, status, updated_at: updatedAt } : item),
  } : current);
}

export function removeCachedInstance(client: QueryClient, instanceID: string) {
  client.removeQueries({ queryKey: ["instances", instanceID], exact: true });
  client.setQueryData<ListResponse<InstanceRecord>>(["instances"], (current) => current ? {
    ...current,
    items: current.items.filter((item) => item.id !== instanceID),
    total: current.total === undefined ? undefined : Math.max(0, current.total - 1),
  } : current);
}
