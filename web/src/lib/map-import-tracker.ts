const changed = "l4d2-map-imports-changed";
const fallback = new Map<string, string[]>();
const memoryOnly = new Set<string>();
const key = (userId: string) => `l4d2-map-imports:${userId}`;

export function pendingMapImports(userId: string): string[] {
  if (memoryOnly.has(userId)) return fallback.get(userId) || [];
  try {
    const value: unknown = JSON.parse(localStorage.getItem(key(userId)) || "[]");
    return Array.isArray(value) ? value.filter((id): id is string => typeof id === "string") : [];
  } catch {
    return fallback.get(userId) || [];
  }
}

function save(userId: string, ids: string[]) {
  fallback.set(userId, ids);
  try {
    localStorage.setItem(key(userId), JSON.stringify(ids));
    memoryOnly.delete(userId);
  } catch {
    memoryOnly.add(userId);
  }
  window.dispatchEvent(new Event(changed));
}

export function trackMapImport(userId: string, jobId: string) {
  save(userId, [...new Set([...pendingMapImports(userId), jobId])]);
}

export function acknowledgeMapImport(userId: string, jobId: string) {
  save(userId, pendingMapImports(userId).filter((id) => id !== jobId));
}

export function subscribeMapImports(listener: () => void) {
  window.addEventListener(changed, listener);
  window.addEventListener("storage", listener);
  return () => {
    window.removeEventListener(changed, listener);
    window.removeEventListener("storage", listener);
  };
}
