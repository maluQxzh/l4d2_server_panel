import type { Job } from "./types";

const diagnosticJobTypes = new Set([
  "a2s.info",
  "a2s.players",
  "rcon.players",
  "check_update",
  "list",
]);

export interface RecentOperation {
  job: Job;
  occurrences: number;
}

export function summarizeRecentOperations(items: Job[], limit = 5): RecentOperation[] {
  const groups: RecentOperation[] = [];
  const completed = new Map<string, RecentOperation>();

  for (const job of items) {
    if (diagnosticJobTypes.has(job.type)) continue;
    if (job.status === "succeeded") {
      const key = `${job.type}\u0000${job.status}\u0000${job.message?.trim() || ""}`;
      const existing = completed.get(key);
      if (existing) {
        existing.occurrences += 1;
        continue;
      }
      const group = { job, occurrences: 1 };
      completed.set(key, group);
      groups.push(group);
      continue;
    }
    // Active, failed and canceled operations remain individually visible even
    // when their labels match: each one can require a different response.
    groups.push({ job, occurrences: 1 });
  }

  return groups.slice(0, limit);
}
