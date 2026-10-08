import type { InvalidateQueryFilters, QueryClient } from "@tanstack/react-query";

// A connected socket does not guarantee that this user receives every resource's
// job events. Keep a bounded fallback without polling as fast as offline mode.
export const liveRefreshInterval = 30_000;

// TanStack reuses a pending first fetch during invalidation, even when that
// request began before the write/event. Cancel those requests before refreshing
// so their stale response cannot become the final cached list. Inactive and
// permission-disabled queries retain normal invalidation behavior.
export function refreshQueries(client: QueryClient, filters: InvalidateQueryFilters) {
  if (filters.refetchType === "none") return client.invalidateQueries(filters);
  const initial = new Set(client.getQueryCache().findAll(filters).filter((query) =>
    query.state.data === undefined && query.state.fetchStatus === "fetching",
  ));
  if (!initial.size) return client.invalidateQueries(filters);
  return client.cancelQueries({ predicate: (query) => initial.has(query) })
    .then(() => client.invalidateQueries(filters));
}
