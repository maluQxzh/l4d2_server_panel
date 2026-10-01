import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord, MapRecord } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "owner" } as { id?: string; role: string; role_bindings?: unknown } }));

vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { MapsPage } from "./MapsPage";

const map: MapRecord = {
  id: "12345678-1234-1234-1234-123456789abc",
  node_id: "87654321-4321-4321-4321-cba987654321",
  name: "Claustrophobia",
  author: "Sean C. Wood",
  version: "1.2",
  description: "L4D2 version of Claustrophobia",
  health: "healthy",
  aggregate_sha256: "4241ce300a7bfdc6aabbccddeeff00112233445566778899aabbccddeeff0011",
  total_size: 188_743_680,
  source_type: "qq_flash",
  source_name: "l4d2_claustrophobia.zip",
  created_by: "map-uploader",
  created_by_display_name: "地图管理员",
  removed_files: 1_182,
  removed_bytes: 245_471_642,
  artifacts: [{ id: "artifact-1", ordinal: 1, size: 188_743_680, sha256: "a".repeat(64) }],
  bsps: [{ virtual_path: "maps/claustrophobia1.bsp", start_name: "claustrophobia1", dictionary_present: true }],
  modes: [{ name: "coop", chapters: [{ ordinal: 1, bsp_name: "claustrophobia1", display_name: "Onyx St. Sewer" }] }],
  assignments: [{
    map_id: "12345678-1234-1234-1234-123456789abc",
    instance_id: "instance-1",
    restart_required: false,
    created_at: "2026-08-07T13:46:00Z",
    updated_at: "2026-08-07T13:46:00Z",
  }],
  created_at: "2026-08-07T13:46:00Z",
  updated_at: "2026-08-07T13:46:00Z",
};

const assignedInstance = {
  id: "instance-1",
  node_id: map.node_id,
  name: "专家战役 #1",
  status: "running",
} as InstanceRecord;

describe("地图管理卡片", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => vi.restoreAllMocks());

  it("按 ID 直接读取并展开地图详情，不依赖列表和名称筛选", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const detail = vi.spyOn(api, "map").mockResolvedValue(map);
    const list = vi.spyOn(api, "maps").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [] });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[`/maps?map=${map.id}`]}><MapsPage /></MemoryRouter></ToastProvider></QueryClientProvider>);
    expect(await screen.findByRole("heading", { name: map.name })).toBeInTheDocument();
    expect(detail).toHaveBeenCalledWith(map.id);
    expect(list).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "收起详情" })).toHaveAttribute("aria-expanded", "true");
  });

  it("地图已删除或失去查看权限时显示错误，不展示旧地图卡片", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(api, "map").mockRejectedValue(new Error("地图不存在或无权查看"));
    vi.spyOn(api, "instances").mockResolvedValue({ items: [] });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[`/maps?map=${map.id}`]}><MapsPage /></MemoryRouter></ToastProvider></QueryClientProvider>);
    expect(await screen.findByRole("alert")).toHaveTextContent("地图不存在或无权查看");
    expect(screen.queryByRole("heading", { name: map.name })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "返回地图库" })).toBeInTheDocument();
  });

  it("使用三项总览并默认折叠次要地图信息", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "maps").mockResolvedValue({ items: [map] });
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [assignedInstance] });
    vi.spyOn(api, "jobs").mockResolvedValue({ items: [] });

    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <MapsPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("heading", { name: "Claustrophobia" })).toBeInTheDocument();
    expect(screen.getByText("逻辑地图")).toBeInTheDocument();
    expect(screen.getByText("地图对象占用空间")).toBeInTheDocument();
    expect(screen.getByText("实例分配", { selector: ".map-summary span" })).toBeInTheDocument();
    expect(screen.queryByText("处理后容量")).not.toBeInTheDocument();
    expect(screen.queryByText("已清理空间")).not.toBeInTheDocument();

    expect(screen.getByText("作者：")).toBeInTheDocument();
    expect(screen.getByText("版本：")).toBeInTheDocument();
    expect(screen.getByText("模式：")).toBeInTheDocument();
    expect(screen.getByText("处理后 VPK 大小：")).toBeInTheDocument();
    expect(screen.queryByText("VPK 部件")).not.toBeInTheDocument();
    expect(screen.queryByText("已清理文件")).not.toBeInTheDocument();
    expect(screen.getByText("Sean C. Wood")).toBeInTheDocument();
    expect(screen.getByText("上传者")).toBeInTheDocument();
    expect(screen.getByText("地图管理员", { selector: ".map-card__uploader strong" })).toBeInTheDocument();
    expect(screen.getByText("1.2")).toBeInTheDocument();
    expect(screen.getByText("合作")).toBeInTheDocument();
    const assignedHeading = screen.getByText("已分配实例");
    expect(assignedHeading).toBeInTheDocument();
    expect(assignedHeading.closest(".map-card__profile")).not.toBeNull();
    expect(document.querySelector(".map-card__snapshot")).toBeNull();
    expect(screen.getByText("专家战役 #1", { selector: ".map-assignment-list--summary strong" })).toBeInTheDocument();
    expect(screen.getByText("已生效")).toBeInTheDocument();

    const toggle = screen.getByRole("button", { name: "展开详情" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(screen.getAllByText("claustrophobia1.bsp")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "收起详情" })).toHaveAttribute("aria-expanded", "true");
  });

  it("可按上传者、已分配实例和未分配状态筛选地图", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const unassignedMap = {
      ...map,
      id: "22345678-1234-1234-1234-123456789abc",
      name: "No Mercy Remix",
      created_by: "other-uploader",
      created_by_display_name: "其他上传者",
      assignments: [],
    };
    vi.spyOn(api, "maps").mockResolvedValue({ items: [map, unassignedMap] });
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [assignedInstance] });
    vi.spyOn(api, "jobs").mockResolvedValue({ items: [] });

    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <MapsPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    expect(await screen.findByRole("heading", { name: "Claustrophobia" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "No Mercy Remix" })).toBeInTheDocument();
    const uploader = screen.getByRole("combobox", { name: "上传者筛选" });
    const instance = screen.getByRole("combobox", { name: "实例筛选" });
    expect(screen.getByRole("option", { name: "地图管理员" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "其他上传者" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "专家战役 #1" })).toBeInTheDocument();
    const unassignedOption = screen.getByRole("option", { name: "未分配" }) as HTMLOptionElement;

    fireEvent.change(uploader, { target: { value: "other-uploader" } });
    expect(screen.queryByRole("heading", { name: "Claustrophobia" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "No Mercy Remix" })).toBeInTheDocument();
    expect(screen.getByText("1 / 2 张地图")).toBeInTheDocument();

    fireEvent.change(uploader, { target: { value: "" } });
    fireEvent.change(instance, { target: { value: assignedInstance.id } });
    expect(screen.getByRole("heading", { name: "Claustrophobia" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "No Mercy Remix" })).not.toBeInTheDocument();

    fireEvent.change(instance, { target: { value: unassignedOption.value } });
    expect(screen.queryByRole("heading", { name: "Claustrophobia" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "No Mercy Remix" })).toBeInTheDocument();
  });

  it("可搜索地图信息并筛选内容相同的地图", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const duplicateMap = { ...map, id: "22345678-1234-1234-1234-123456789abc", name: "CLAUSTROPHOBIA", assignments: [] };
    const uniqueMap = { ...map, id: "32345678-1234-1234-1234-123456789abc", name: "No Mercy Remix", aggregate_sha256: "9".repeat(64), assignments: [], bsps: [], modes: [] };
    vi.spyOn(api, "maps").mockResolvedValue({ items: [map, duplicateMap, uniqueMap] });
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [assignedInstance] });
    vi.spyOn(api, "jobs").mockResolvedValue({ items: [] });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><MapsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "No Mercy Remix" })).toBeInTheDocument();
    const search = screen.getByRole("searchbox", { name: "搜索地图" });
    fireEvent.change(search, { target: { value: "Onyx St. Sewer" } });
    expect(screen.getByRole("heading", { name: "Claustrophobia" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "No Mercy Remix" })).not.toBeInTheDocument();

    fireEvent.change(search, { target: { value: "" } });
    fireEvent.change(screen.getByRole("combobox", { name: "重复地图筛选" }), { target: { value: "content" } });
    expect(screen.getAllByText("内容相同 · 共 2 张")).toHaveLength(2);
    expect(screen.getByText("360.0 MB")).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { name: /Claustrophobia/i })).toHaveLength(2);
    expect(screen.queryByRole("heading", { name: "No Mercy Remix" })).not.toBeInTheDocument();
  });

  it("运行中实例未配置 RCON 时确认后仍继续分配", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "maps").mockResolvedValue({ items: [{ ...map, assignments: [] }] });
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [{ ...assignedInstance, rcon_configured: false }] });
    vi.spyOn(api, "jobs").mockResolvedValue({ items: [] });
    const assign = vi.spyOn(api, "assignMap").mockResolvedValue({ job_id: "map-assign" });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);

    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RealtimeProvider enabled={false}>
            <MemoryRouter>
              <MapsPage />
            </MemoryRouter>
          </RealtimeProvider>
        </ToastProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "分配实例" }));
    fireEvent.click(screen.getByRole("checkbox"));
    expect(screen.getByText("运行中 · 需手动输入 !update_vpk")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "提交变更" }));
    await waitFor(() => expect(confirm).toHaveBeenCalledWith(expect.stringContaining("!update_vpk")));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(map.id, assignedInstance.id));
  });

  it("只读地图绑定不显示导入、分配、校验或删除操作", async () => {
    authState.user = {
      role: "viewer",
      role_bindings: [{
        role: "viewer",
        scope_type: "node",
        node_id: map.node_id,
        permissions: ["nodes.read", "maps.read"],
      }],
    };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "maps").mockResolvedValue({ items: [map] });
    vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    const instances = vi.spyOn(api, "instances").mockResolvedValue({ items: [assignedInstance] });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><MapsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "Claustrophobia" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /QQ 闪传导入/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "分配实例" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "校验" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "永久删除" })).not.toBeInTheDocument();
    expect(instances).not.toHaveBeenCalled();
  });

  it("实例范围可管理所在节点地图但只能删除自己导入的地图", async () => {
    authState.user = {
      id: "map-user",
      role: "admin",
      role_bindings: [{
        role: "admin",
        scope_type: "instance",
        instance_id: assignedInstance.id,
        permissions: ["instances.read", "maps.read", "maps.import", "maps.assign", "maps.verify", "maps.delete"],
      }],
    };
    const ownMap = { ...map, assignments: [], created_by: "map-user" };
    const otherMap = { ...map, id: "22345678-1234-1234-1234-123456789abc", name: "Other map", assignments: [], created_by: "other-user" };
    const scopedInstance = { ...assignedInstance, node_name: "共享地图节点", node_status: "online" as const };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    vi.spyOn(api, "maps").mockResolvedValue({ items: [ownMap, otherMap] });
    const nodes = vi.spyOn(api, "nodes").mockResolvedValue({ items: [] });
    vi.spyOn(api, "instances").mockResolvedValue({ items: [scopedInstance] });
    const importMap = vi.spyOn(api, "importMap").mockResolvedValue({ job_id: "map-import-gbk" });

    render(<QueryClientProvider client={client}><ToastProvider><RealtimeProvider enabled={false}><MemoryRouter><MapsPage /></MemoryRouter></RealtimeProvider></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "Other map" })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "分配实例" })).toHaveLength(2);
    expect(screen.getAllByRole("button", { name: "校验" })).toHaveLength(2);
    expect(screen.getAllByRole("button", { name: "永久删除" })).toHaveLength(1);
    expect(nodes).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /QQ 闪传导入/ }));
    expect(await screen.findByRole("option", { name: "共享地图节点" })).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: /^ZIP \/ VPK 路径编码/ })).not.toBeInTheDocument();
    fireEvent.change(screen.getByRole("combobox", { name: "目标节点" }), { target: { value: map.node_id } });
    fireEvent.change(screen.getByRole("textbox", { name: /^QQ 闪传链接/ }), { target: { value: "https://qfile.qq.com/q/LegacyGBK" } });
    fireEvent.click(screen.getByRole("button", { name: "开始导入" }));
    await waitFor(() => expect(importMap).toHaveBeenCalledWith(map.node_id, "https://qfile.qq.com/q/LegacyGBK"));
  });
});
