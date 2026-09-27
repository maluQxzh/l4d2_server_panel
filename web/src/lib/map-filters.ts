import type { MapRecord } from "./types";

export type MapDuplicateFilter = "" | "content" | "name";

function normalizedText(value: string) {
  return value.trim().replace(/\s+/g, " ").toLowerCase();
}

export function mapMatchesSearch(item: MapRecord, search: string) {
  const query = normalizedText(search);
  if (!query) return true;
  const fields = [
    item.name, item.author, item.version, item.description, item.source_name,
    item.created_by_display_name, item.aggregate_sha256,
    ...(item.bsps || []).flatMap((bsp) => [bsp.virtual_path, bsp.start_name]),
    ...(item.modes || []).flatMap((mode) => mode.chapters.flatMap((chapter) => [chapter.display_name, chapter.bsp_name])),
  ];
  return fields.some((value) => value && normalizedText(value).includes(query));
}

// Calculate groups before search/uploader/assignment filters so narrowing a
// duplicate group down to one visible card does not hide that card. Objects
// on different nodes are kept separate because their map roots are separate.
export function mapDuplicateCounts(items: MapRecord[], filter: MapDuplicateFilter) {
  const counts = new Map<string, number>();
  const keys = new Map<string, string>();
  if (!filter) return counts;
  for (const item of items) {
    const identity = filter === "content" ? item.aggregate_sha256.trim().toLowerCase() : normalizedText(item.name);
    if (!identity) continue;
    const key = `${item.node_id}:${identity}`;
    keys.set(item.id, key);
    counts.set(key, (counts.get(key) || 0) + 1);
  }
  return new Map([...keys].map(([id, key]) => [id, counts.get(key)!]));
}

export function mapDeduplicatedSize(items: MapRecord[]) {
  const seen = new Set<string>();
  return items.reduce((size, item) => {
    const replicas = item.replicas || [];
    const artifacts = item.artifacts || [];
    if (!replicas.length || !artifacts.length) {
      const key = `${item.node_id}:${item.aggregate_sha256.trim().toLowerCase()}`;
      if (seen.has(key)) return size;
      seen.add(key);
      return size + item.total_size;
    }
    return replicas.reduce((replicaSize, replica) => {
      if (replica.health === "missing") return replicaSize;
      return artifacts.reduce((artifactSize, artifact) => {
        const key = `${item.node_id}:${replica.filesystem_id}:${artifact.sha256}`;
        if (seen.has(key)) return artifactSize;
        seen.add(key);
        return artifactSize + artifact.size;
      }, replicaSize);
    }, size);
  }, 0);
}
