import { liveRefreshInterval } from "./query-refresh";

export function activeTask(status?: string) {
  return status !== undefined && ["queued", "dispatched", "running"].includes(status);
}

export function preparedPollInterval(data?: { supported: boolean; items: { status: string; busy?: boolean; consume_status?: string }[]; queues?: { status: string }[] }) {
  if (data && !data.supported && !data.items.length && !data.queues?.length) return false;
  return data?.items.some((item) => activeTask(item.status) || activeTask(item.consume_status) || item.busy)
    || data?.queues?.some((queue) => activeTask(queue.status)) ? 3000 : liveRefreshInterval;
}

export function scanPollInterval(data?: { scan?: { status: string; busy?: boolean } | null }) {
  return data?.scan && (data.scan.busy || activeTask(data.scan.status)) ? 3000 : liveRefreshInterval;
}
