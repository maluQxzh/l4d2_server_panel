import { describe, expect, it } from "vitest";
import { filterAndSortPackages } from "./package-list";
import type { PackageRecord } from "./types";

const packages: PackageRecord[] = [
  { id: "b", name: "Beta 10.vpk", kind: "vpk", enabled: true, installed_at: "2026-08-02T00:00:00Z" },
  { id: "a", name: "alpha.vpk", kind: "vpk", enabled: true, installed_at: "2026-08-01T00:00:00Z" },
  { id: "c", name: "Beta 2.vpk", kind: "vpk", enabled: false },
  { id: "sm", name: "SourceMod", kind: "sourcemod", enabled: true, installed_at: "2026-08-03T00:00:00Z" },
  { id: "mm", name: "Metamod:Source", kind: "metamod", enabled: true, installed_at: "2026-08-04T00:00:00Z" },
];

describe("模组清单搜索与排序", () => {
  it("只按名称执行不区分大小写的搜索", () => {
    expect(filterAndSortPackages(packages, " BETA ", "name_asc").map((item) => item.id)).toEqual(["c", "b"]);
  });

  it("支持添加时间和名称顺序且不修改原清单", () => {
    expect(filterAndSortPackages(packages, "", "installed_desc").map((item) => item.id)).toEqual(["mm", "sm", "b", "a", "c"]);
    expect(filterAndSortPackages(packages, "", "installed_asc").map((item) => item.id)).toEqual(["mm", "sm", "a", "b", "c"]);
    expect(filterAndSortPackages(packages, "", "name_desc").map((item) => item.id)).toEqual(["mm", "sm", "b", "c", "a"]);
    expect(packages.map((item) => item.id)).toEqual(["b", "a", "c", "sm", "mm"]);
  });

  it("Metamod 和 SourceMod 在每种排序下都固定置顶", () => {
    for (const sort of ["installed_desc", "installed_asc", "name_asc", "name_desc"] as const) {
      expect(filterAndSortPackages(packages, "", sort).slice(0, 2).map((item) => item.id)).toEqual(["mm", "sm"]);
    }
  });
});
