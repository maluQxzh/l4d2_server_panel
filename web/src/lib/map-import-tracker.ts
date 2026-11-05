const changed = "l4d2-map-imports-changed";
export const mapImportTrackingTTL = 7 * 24 * 60 * 60 * 1000;
export const maxTrackedMapImports = 64;
type Entry = { id: string; trackedAt: number };
const fallback = new Map<string, Entry[]>();
const memoryOnly = new Set<string>();
const key = (userId: string) => `l4d2-map-imports:${userId}`;

export function pendingMapImports(userId: string): string[] {
  return entriesFor(userId).map((item) => item.id);
}

function entriesFor(userId: string): Entry[] {
  let value: unknown = fallback.get(userId) || [];
  if (!memoryOnly.has(userId)) {
    try { value = JSON.parse(localStorage.getItem(key(userId)) || "[]"); } catch { /* Keep bounded memory fallback. */ }
  }
  const now = Date.now();
  const entries = new Map<string, Entry>();
  if (Array.isArray(value)) for (const raw of value) {
    const item = typeof raw === "string" ? { id: raw, trackedAt: now } : raw;
    if (item && typeof item.id === "string" && item.id.length > 0 && item.id.length <= 128
      && Number.isFinite(item.trackedAt) && item.trackedAt <= now && now - item.trackedAt < mapImportTrackingTTL) {
      entries.set(item.id, { id: item.id, trackedAt: item.trackedAt });
    }
  }
  const bounded = [...entries.values()].sort((a, b) => a.trackedAt - b.trackedAt).slice(-maxTrackedMapImports);
  if (JSON.stringify(value) !== JSON.stringify(bounded)) save(userId, bounded, false);
  return bounded;
}

function save(userId: string, ids: Entry[], notify = true) {
  fallback.set(userId, ids);
  try {
    localStorage.setItem(key(userId), JSON.stringify(ids));
    memoryOnly.delete(userId);
  } catch {
    memoryOnly.add(userId);
  }
  if (notify) window.dispatchEvent(new Event(changed));
}

export function trackMapImport(userId: string, jobId: string) {
  if (!jobId || jobId.length > 128) return;
  const entries = entriesFor(userId);
  if (!entries.some((item) => item.id === jobId)) entries.push({ id: jobId, trackedAt: Date.now() });
  save(userId, entries.slice(-maxTrackedMapImports));
}

export function acknowledgeMapImport(userId: string, jobId: string) {
  save(userId, entriesFor(userId).filter((item) => item.id !== jobId));
}

export function subscribeMapImports(listener: () => void) {
  window.addEventListener(changed, listener);
  window.addEventListener("storage", listener);
  const timer = window.setInterval(listener, 60_000);
  return () => {
    window.removeEventListener(changed, listener);
    window.removeEventListener("storage", listener);
    window.clearInterval(timer);
  };
}
