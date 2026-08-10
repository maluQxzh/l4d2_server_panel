import type { PackageRecord } from "./types";

export type PackageSort = "installed_desc" | "installed_asc" | "name_asc" | "name_desc";

function compareNames(left: PackageRecord, right: PackageRecord) {
  return left.name.localeCompare(right.name, "zh-CN", { numeric: true, sensitivity: "base" })
    || left.id.localeCompare(right.id);
}

function installedAt(item: PackageRecord) {
  const timestamp = item.installed_at ? Date.parse(item.installed_at) : Number.NaN;
  return Number.isFinite(timestamp) ? timestamp : undefined;
}

export function filterAndSortPackages(items: PackageRecord[], query: string, sort: PackageSort) {
  const normalizedQuery = query.trim().toLocaleLowerCase();
  return items
    .filter((item) => !normalizedQuery || item.name.toLocaleLowerCase().includes(normalizedQuery))
    .sort((left, right) => {
      if (sort === "name_asc") return compareNames(left, right);
      if (sort === "name_desc") return -compareNames(left, right);
      const leftTime = installedAt(left);
      const rightTime = installedAt(right);
      if (leftTime === undefined && rightTime !== undefined) return 1;
      if (leftTime !== undefined && rightTime === undefined) return -1;
      if (leftTime !== undefined && rightTime !== undefined && leftTime !== rightTime) {
        return sort === "installed_asc" ? leftTime - rightTime : rightTime - leftTime;
      }
      return compareNames(left, right);
    });
}
