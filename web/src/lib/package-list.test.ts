import { describe, expect, it } from "vitest";
import { filterAndSortPackages } from "./package-list";
import type { PackageRecord } from "./types";

const packages: PackageRecord[] = [
  { id: "b", name: "Beta 10.vpk", kind: "vpk", enabled: true, installed_at: "2026-08-02T00:00:00Z" },
  { id: "a", name: "alpha.vpk", kind: "vpk", enabled: true, installed_at: "2026-08-01T00:00:00Z" },
  { id: "c", name: "Beta 2.vpk", kind: "vpk", enabled: false },
];

describe("模组清单搜索与排序", () => {
  it("只按名称执行不区分大小写的搜索", () => {
    expect(filterAndSortPackages(packages, " BETA ", "name_asc").map((item) => item.id)).toEqual(["c", "b"]);
  });

  it("支持添加时间和名称顺序且不修改原清单", () => {
    expect(filterAndSortPackages(packages, "", "installed_desc").map((item) => item.id)).toEqual(["b", "a", "c"]);
    expect(filterAndSortPackages(packages, "", "installed_asc").map((item) => item.id)).toEqual(["a", "b", "c"]);
    expect(filterAndSortPackages(packages, "", "name_desc").map((item) => item.id)).toEqual(["b", "c", "a"]);
    expect(packages.map((item) => item.id)).toEqual(["b", "a", "c"]);
  });
});
