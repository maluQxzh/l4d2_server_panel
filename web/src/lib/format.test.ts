import { describe, expect, it, vi } from "vitest";
import { bytes, duration, jobLabel, jobLabels, percent, relativeTime, statusLabels } from "./format";

describe("格式化工具", () => {
  it("安全格式化容量与百分比", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(1024 ** 3)).toBe("1.0 GB");
    expect(percent(95, 100)).toBe(95);
    expect(percent(10, 0)).toBe(0);
    expect(percent(150, 100)).toBe(100);
  });

  it("生成中文相对时间与运行时长", () => {
    vi.setSystemTime(new Date("2026-07-31T12:00:00Z"));
    expect(relativeTime("2026-07-31T11:30:00Z")).toBe("30 分钟前");
    expect(duration("2026-07-30T10:00:00Z")).toBe("1 天 2 小时");
  });

  it("包含核心状态的中文标签", () => {
    expect(statusLabels.running).toBe("运行中");
    expect(statusLabels.failed).toBe("失败");
    expect(jobLabels["backup.upload.abort"]).toBe("清理上传暂存");
    expect(jobLabels["backup.delete"]).toBe("永久删除备份");
  });

  it("按安全展示类型或包类型区分插件和模组安装", () => {
    expect(jobLabel({ type: "install_mod", display_type: "install_plugin" })).toBe("安装插件");
    expect(jobLabel({ type: "install_mod", payload: { kind: "sourcemod" } })).toBe("安装插件");
    expect(jobLabel({ type: "install_mod", payload: { kind: "workshop" } })).toBe("安装模组");
  });
});
