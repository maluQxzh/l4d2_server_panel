import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "../context/RealtimeContext";
import { ToastProvider } from "../context/ToastContext";
import { api } from "../lib/api";
import type { InstanceRecord } from "../lib/types";

const authState = vi.hoisted(() => ({ user: { role: "owner" } as { role: string; role_bindings?: unknown } }));

vi.mock("../context/AuthContext", () => ({ useAuth: () => authState }));

import { InstanceDetailPage } from "./InstanceDetailPage";

const instance: InstanceRecord = {
  id: "12345678-1234-1234-1234-123456789abc",
  node_id: "87654321-4321-4321-4321-cba987654321",
  node_name: "广州游戏节点 A",
  node_status: "online",
  node_public_address: "game.example.com",
  name: "绝境求生",
  status: "stopped",
  host: "0.0.0.0",
  port: 27015,
  tv_port: 27020,
  players: 0,
  max_players: 8,
  update_available: false,
  rcon_configured: true,
  version: 4,
  config: {
    hostname: "绝境求生",
    mode: "coop",
    difficulty: "normal",
    max_players: 8,
    start_map: "c2m1_highway",
    public_status: false,
    auto_restart: true,
  },
  created_at: "2026-08-07T00:00:00Z",
  updated_at: "2026-08-07T00:00:00Z",
};

function renderConfigPage(item: InstanceRecord) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(api, "instance").mockResolvedValue(item);
  vi.spyOn(api, "maps").mockResolvedValue({ items: [] });
  const router = createMemoryRouter(
    [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
    { initialEntries: [`/instances/${item.id}?tab=config`] },
  );
  return render(
    <QueryClientProvider client={client}>
      <RealtimeProvider enabled={false}>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </RealtimeProvider>
    </QueryClientProvider>,
  );
}

function renderPackagesPage(item: InstanceRecord, tab = "mods") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(api, "instance").mockResolvedValue(item);
  const router = createMemoryRouter(
    [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
    { initialEntries: [`/instances/${item.id}?tab=${tab}`] },
  );
  return render(
    <QueryClientProvider client={client}>
      <RealtimeProvider enabled={false}>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </RealtimeProvider>
    </QueryClientProvider>,
  );
}

function renderPlayersPage(item: InstanceRecord) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  vi.spyOn(api, "instance").mockResolvedValue(item);
  vi.spyOn(api, "bans").mockResolvedValue({ items: [] });
  const router = createMemoryRouter(
    [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
    { initialEntries: [`/instances/${item.id}?tab=players`] },
  );
  return render(
    <QueryClientProvider client={client}>
      <RealtimeProvider enabled={false}>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </RealtimeProvider>
    </QueryClientProvider>,
  );
}

describe("实例在线玩家", () => {
  beforeEach(() => {
    authState.user = {
      role: "operator",
      role_bindings: [{
        role: "operator",
        scope_type: "instance",
        instance_id: instance.id,
        permissions: ["instances.read", "players.read"],
      }],
    };
  });
  afterEach(() => { vi.restoreAllMocks(); });

  it("空缓存异步刷新完成前保持加载，并且不依赖作业读取权限", async () => {
    const running = { ...instance, status: "running" as const, players: 1 };
    let requests = 0;
    vi.spyOn(api, "players").mockImplementation(async () => {
      requests += 1;
      if (requests === 1) return { items: [], status: "refreshing", refresh_job_id: "11111111-1111-4111-8111-111111111111" };
      return {
        items: [{ name: "Coach", steam_id: "STEAM_1:1:12345", score: 42, ping: 35 }],
        status: "ready",
        fresh_at: "2026-08-16T10:23:00Z",
      };
    });
    const jobs = vi.spyOn(api, "instanceJobs");

    renderPlayersPage(running);

    expect(await screen.findByText("正在查询最新玩家信息…")).toBeVisible();
    expect(screen.queryByText("服务器里暂时没人")).not.toBeInTheDocument();
    expect(await screen.findByText("Coach", {}, { timeout: 2_500 })).toBeVisible();
    expect(requests).toBeGreaterThanOrEqual(2);
    expect(jobs).not.toHaveBeenCalled();
  });
});

describe("游戏实例配置中的端口", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
  });

  it("停止状态把新端口随配置一起提交", async () => {
    const update = vi.spyOn(api, "updateInstance").mockResolvedValue({ instance: { ...instance, version: 5 } });
    renderConfigPage(instance);

    const port = await screen.findByLabelText(/游戏端口/);
    expect(port).toBeEnabled();
    fireEvent.change(port, { target: { value: "27016" } });
    fireEvent.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(instance.id, expect.objectContaining({
      version: 4,
      port: 27016,
      config: expect.objectContaining({ hostname: "绝境求生" }),
    })));
  });

  it("运行中只禁用端口，仍允许保存其他配置", async () => {
    const running = { ...instance, status: "running" as const };
    const update = vi.spyOn(api, "updateInstance").mockResolvedValue({ instance: { ...running, version: 5 } });
    renderConfigPage(running);

    expect(await screen.findByLabelText(/游戏端口/)).toBeDisabled();
    fireEvent.change(screen.getByLabelText("服务器名称"), { target: { value: "运行中改名" } });
    fireEvent.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(instance.id, expect.not.objectContaining({ port: expect.anything() })));
  });

  it("只显示已授权标签，且创建备份不依赖备份清单权限", async () => {
    authState.user = {
      role: "operator",
      role_bindings: [{
        role: "operator",
        scope_type: "instance",
        instance_id: instance.id,
        permissions: ["instances.read", "backups.create"],
      }],
    };
    localStorage.setItem(`l4d2panel.backup-upload.v1:${instance.id}`, JSON.stringify({
      version: 1,
      instanceId: instance.id,
      createKey: "persisted-create-key",
      uploadId: "22222222-2222-4222-8222-222222222222",
      name: "old.zip",
      size: 1024,
      sha256: "a".repeat(64),
      platform: "linux/amd64",
      updatedAt: "2026-08-09T00:00:00Z",
    }));
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    const backups = vi.spyOn(api, "backups").mockResolvedValue({ items: [] });
    const reconcileUpload = vi.spyOn(api, "backupUpload").mockRejectedValue(new Error("must not be queried"));
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=backups`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "实例备份" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "立即备份" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "控制台" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "玩家" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "配置" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "文件" })).not.toBeInTheDocument();
    expect(backups).not.toHaveBeenCalled();
    expect(reconcileUpload).not.toHaveBeenCalled();
  });

  it("从备份列表确认后提交永久删除任务", async () => {
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    vi.spyOn(api, "backups").mockResolvedValue({ items: [{
      id: "backup-id",
      name: "manual-20260812T120000Z-job.zip",
      platform: "windows/amd64",
      size: 1024,
      checksum: "a".repeat(64),
      created_at: "2026-08-12T12:00:00Z",
    }] });
    const remove = vi.spyOn(api, "deleteBackup").mockResolvedValue({ job_id: "delete-job" });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=backups`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("button", { name: "删除" }));

    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("manual-20260812T120000Z-job.zip"));
    await waitFor(() => expect(remove).toHaveBeenCalledWith(instance.id, "backup-id", expect.any(String)));
    expect(await screen.findByText("备份删除任务已提交")).toBeVisible();
  });

  it("创建插件备份时可选择精简路径布局", async () => {
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    const backups = vi.spyOn(api, "backups").mockResolvedValue({ items: [{
      id: "plugin-backup-id",
      name: "plugin-20260812T120000Z-job.7z",
      kind: "plugin_preserve",
      platform: "linux/amd64",
      size: 2048,
      checksum: "b".repeat(64),
      created_at: "2026-08-12T12:00:00Z",
    }] });
    const create = vi.spyOn(api, "createPluginBackup").mockResolvedValue({ job_id: "plugin-backup-job" });
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=backups`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "插件备份" })).toBeVisible();
    await waitFor(() => expect(backups).toHaveBeenCalledWith(instance.id));
    expect((await screen.findByText("plugin-20260812T120000Z-job.7z")).closest(".backup-row")).toHaveTextContent("保留完整游戏路径");
    fireEvent.click(screen.getByRole("button", { name: "备份插件" }));
    fireEvent.click(screen.getByRole("radio", { name: /不保留原路径/ }));
    fireEvent.click(screen.getByRole("button", { name: "开始备份" }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(instance.id, "compact"));
    expect(await screen.findByText("插件备份任务已提交")).toBeVisible();
  });
});

describe("实例文件管理", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => { vi.restoreAllMocks(); });

  it("从根目录打开并在异步列表完成前保持加载状态", async () => {
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    let addonsRequests = 0;
    vi.spyOn(api, "files").mockImplementation(async (_instanceID, path = "") => {
      if (path === "left4dead2/addons") {
        addonsRequests += 1;
        if (addonsRequests === 1) return { items: [], shortcuts: [], status: "loading", refresh_job_id: "list-job" };
        return { items: [{ name: "sourcemod", path: "left4dead2/addons/sourcemod", type: "directory" }], shortcuts: [], status: "ready" };
      }
      return {
        items: [],
        shortcuts: [{ name: "admins_simple.ini", path: "left4dead2/addons/sourcemod/configs/admins_simple.ini", type: "file", size: 12 }],
        status: "ready",
      };
    });
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=files`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    expect(await screen.findByRole("button", { name: "根目录" })).toBeVisible();
    const addons = screen.getByRole("button", { name: "addons" });
    const sidebar = addons.parentElement!;
    expect(within(sidebar).getAllByRole("button").map((button) => button.textContent)).toEqual([
      "addons", "cfg", "server.cfg", "admins_simple.ini",
    ]);

    fireEvent.click(addons);
    expect(await screen.findByText("正在读取目录内容…")).toBeVisible();
    expect(screen.queryByText("目录为空")).not.toBeInTheDocument();
    expect(await screen.findByRole("button", { name: /sourcemod/ }, { timeout: 2_500 })).toBeVisible();
    expect(addonsRequests).toBeGreaterThanOrEqual(2);
  });

  it("重新验证目录后动态显示后来创建的 admins_simple.ini", async () => {
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    let adminsExists = false;
    vi.spyOn(api, "files").mockImplementation(async () => ({
      items: [],
      shortcuts: adminsExists ? [{ name: "admins_simple.ini", path: "left4dead2/addons/sourcemod/configs/admins_simple.ini", type: "file" as const }] : [],
      status: "ready" as const,
    }));
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=files`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    await screen.findByRole("button", { name: "根目录" });
    expect(screen.queryByRole("button", { name: "admins_simple.ini" })).not.toBeInTheDocument();
    adminsExists = true;
    await client.invalidateQueries({ queryKey: ["instances", instance.id, "files"] });
    expect(await screen.findByRole("button", { name: "admins_simple.ini" })).toBeVisible();
  });

  it("文件刷新任务完成后自动替换首次读取到的旧内容", async () => {
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    vi.spyOn(api, "files").mockResolvedValue({
      items: [{ name: "motd.txt", path: "cfg/motd.txt", type: "file", size: 0 }],
      shortcuts: [],
      status: "ready",
    });
    let reads = 0;
    vi.spyOn(api, "fileContent").mockImplementation(async () => {
      reads += 1;
      if (reads === 1) return { path: "cfg/motd.txt", content: "", refresh_job_id: "read-job" };
      return { path: "cfg/motd.txt", content: "hostname ready\n", sha256: "a".repeat(64) };
    });
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=files`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("button", { name: /motd\.txt/ }));
    expect(await screen.findByText("正在从节点安全读取文件…")).toBeVisible();
    await waitFor(() => expect(reads).toBeGreaterThanOrEqual(2), { timeout: 2_500 });
    await waitFor(() => expect(screen.getByRole("textbox", { name: /文件内容/ })).toHaveValue("hostname ready\n"));
  });
});

describe("游戏实例模组扫描", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => { vi.restoreAllMocks(); });

  it("从模组页一键提交固定目录扫描任务", async () => {
    vi.spyOn(api, "instance").mockResolvedValue(instance);
    vi.spyOn(api, "packages").mockResolvedValue({ items: [] });
    const scan = vi.spyOn(api, "scanInstalledPackages").mockResolvedValue({ job_id: "scan-job" });
    const router = createMemoryRouter(
      [{ path: "/instances/:instanceId", element: <InstanceDetailPage /> }],
      { initialEntries: [`/instances/${instance.id}?tab=mods`] },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><RealtimeProvider enabled={false}><ToastProvider><RouterProvider router={router} /></ToastProvider></RealtimeProvider></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("button", { name: "扫描已安装" }));

    await waitFor(() => expect(scan).toHaveBeenCalledWith(instance.id));
    expect(await screen.findByText(/扫描任务已提交/)).toBeInTheDocument();
  });

  it("可按名称搜索和排序，并阻止运行中停用模组", async () => {
    vi.spyOn(api, "packages").mockResolvedValue({ items: [
      { id: "alpha", name: "Alpha.vpk", kind: "vpk", enabled: true, path: "left4dead2/addons/Alpha.vpk", installed_at: "2026-08-01T00:00:00Z" },
      { id: "beta", name: "Beta.vpk", kind: "vpk", enabled: false, path: "left4dead2/addons/panel-disabled/Beta.vpk", installed_at: "2026-08-02T00:00:00Z" },
    ] });
    vi.spyOn(api, "packageVersions").mockResolvedValue({ items: [] });
    renderPackagesPage({ ...instance, status: "running" });

    expect(await screen.findByText(/当前实例未停止，不能停用已启用的模组/)).toBeVisible();
    expect(await screen.findByRole("button", { name: "停用 Alpha.vpk" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "启用 Beta.vpk" })).toBeEnabled();
    expect(screen.getByLabelText("模组排序")).toHaveValue("installed_desc");

    fireEvent.change(screen.getByLabelText("按名称搜索模组"), { target: { value: "beta" } });
    expect(screen.queryByText("Alpha.vpk", { exact: true })).not.toBeInTheDocument();
    expect(screen.getByText("Beta.vpk", { exact: true })).toBeVisible();
  });
});

describe("服务端组件依赖", () => {
  beforeEach(() => { authState.user = { role: "owner" }; });
  afterEach(() => { vi.restoreAllMocks(); });

  it("未安装 Metamod 时阻止提交 SourceMod 安装", async () => {
    vi.spyOn(api, "packages").mockResolvedValue({ items: [] });
    renderPackagesPage(instance, "plugins");

    fireEvent.click(await screen.findByRole("button", { name: "安装插件" }));
    expect(screen.getByText(/单个文件上限 100 MiB/)).toBeVisible();
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "sourcemod" } });

    expect(screen.getByText("请先安装 Metamod:Source")).toBeVisible();
    expect(screen.getByRole("button", { name: "提交安装" })).toBeDisabled();
  });

  it("固定核心组件排序并从详情查看和编辑智能插件文件", async () => {
    const smart = { id: "smart", name: "Autoscar", kind: "smart" as const, enabled: true, sha256: "a".repeat(64), file_count: 2, installed_at: "2026-08-04T00:00:00Z" };
	const remove = vi.spyOn(api, "deletePackage").mockResolvedValue({ job_id: "delete-component" });
	vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.spyOn(api, "packages").mockResolvedValue({ items: [
      smart,
      { id: "sm", name: "SourceMod", kind: "sourcemod", enabled: true, sha256: "d".repeat(64), file_count: 120, installed_at: "2026-08-02T00:00:00Z" },
      { id: "mm", name: "Metamod:Source", kind: "metamod", enabled: true, sha256: "e".repeat(64), file_count: 8, installed_at: "2026-08-01T00:00:00Z" },
    ] });
    vi.spyOn(api, "packageDetails").mockResolvedValue({ package: smart, files: [
      { package_id: "smart", instance_id: instance.id, path: "left4dead2/cfg/sourcemod/autoscar.cfg", sha256: "b".repeat(64), size: 12, entrypoint: false, editable: true },
      { package_id: "smart", instance_id: instance.id, path: "left4dead2/addons/sourcemod/plugins/autoscar.smx", sha256: "c".repeat(64), size: 20, entrypoint: true, editable: false },
    ] });
    vi.spyOn(api, "packageFileContent").mockResolvedValue({ path: "left4dead2/cfg/sourcemod/autoscar.cfg", content: "enabled = 1", size: 11, sha256: "b".repeat(64) });
    renderPackagesPage(instance, "plugins");

    const detailButtons = await screen.findAllByRole("button", { name: /^查看 .* 详情$/ });
    expect(detailButtons.map((button) => button.getAttribute("aria-label"))).toEqual([
      "查看 Metamod:Source 详情", "查看 SourceMod 详情", "查看 Autoscar 详情",
    ]);
    fireEvent.change(screen.getByLabelText("插件排序"), { target: { value: "name_desc" } });
    expect(screen.getAllByRole("button", { name: /^查看 .* 详情$/ }).map((button) => button.getAttribute("aria-label"))).toEqual([
      "查看 Metamod:Source 详情", "查看 SourceMod 详情", "查看 Autoscar 详情",
    ]);
	expect(screen.getByRole("button", { name: "删除 Metamod:Source" })).toBeEnabled();
	expect(screen.getByRole("button", { name: "删除 SourceMod" })).toBeEnabled();
	fireEvent.click(screen.getByRole("button", { name: "删除 SourceMod" }));
	await waitFor(() => expect(remove).toHaveBeenCalledWith(instance.id, "sm"));
	expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("admins_simple.ini"));

    fireEvent.click(screen.getByRole("button", { name: "查看 Autoscar 详情" }));
    expect(await screen.findByText("left4dead2/cfg/sourcemod/autoscar.cfg")).toBeVisible();
    expect(screen.getByText("left4dead2/addons/sourcemod/plugins/autoscar.smx")).toBeVisible();
    expect(screen.getByText("受包管理保护")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "编辑" }));
    expect(await screen.findByText("编辑 Autoscar 文件")).toBeVisible();
    expect(await screen.findByDisplayValue("enabled = 1")).toBeVisible();
  });
});
