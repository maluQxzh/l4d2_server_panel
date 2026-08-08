import type { InstanceRecord, Permission, Role, RoleBinding, User } from "./types";

type PermissionClass = "read" | "operate" | "manage" | "root";
type PermissionScope = "global" | "node" | "instance" | "resource";

export interface PermissionDefinition {
  id: Permission;
  label: string;
  group: string;
  class: PermissionClass;
  scope: PermissionScope;
  description?: string;
}

export const permissionCatalog: PermissionDefinition[] = [
  { id: "nodes.read", label: "查看节点", group: "节点与运行环境", class: "read", scope: "node" },
  { id: "nodes.enroll", label: "创建节点注册令牌", group: "节点与运行环境", class: "manage", scope: "global" },
  { id: "nodes.update", label: "修改节点资料", group: "节点与运行环境", class: "manage", scope: "node" },
  { id: "nodes.credentials.rotate", label: "轮换节点凭据", group: "节点与运行环境", class: "manage", scope: "node" },
  { id: "nodes.discover", label: "执行节点发现", group: "节点与运行环境", class: "manage", scope: "node" },
  { id: "nodes.quarantine.read", label: "查看隔离目录", group: "节点与运行环境", class: "manage", scope: "node" },
  { id: "nodes.quarantine.purge", label: "永久清理隔离目录", group: "节点与运行环境", class: "manage", scope: "node", description: "不可恢复的高风险操作" },
  { id: "nodes.delete", label: "删除节点", group: "节点与运行环境", class: "manage", scope: "node" },
  { id: "environments.read", label: "查看运行环境", group: "节点与运行环境", class: "read", scope: "node" },
  { id: "environments.manage", label: "管理 SteamCMD 与游戏安装", group: "节点与运行环境", class: "manage", scope: "node" },

  { id: "instances.read", label: "查看实例", group: "实例", class: "read", scope: "instance" },
  { id: "instances.create", label: "创建实例", group: "实例", class: "manage", scope: "node" },
  { id: "instances.configure", label: "修改实例配置", group: "实例", class: "operate", scope: "instance" },
  { id: "instances.delete", label: "删除或解除实例绑定", group: "实例", class: "manage", scope: "instance" },
  { id: "instances.start", label: "启动实例", group: "实例生命周期", class: "operate", scope: "instance" },
  { id: "instances.stop", label: "停止实例", group: "实例生命周期", class: "operate", scope: "instance" },
  { id: "instances.restart", label: "重启实例", group: "实例生命周期", class: "operate", scope: "instance" },
  { id: "instances.update", label: "更新实例", group: "实例生命周期", class: "operate", scope: "instance" },
  { id: "instances.validate", label: "校验实例", group: "实例生命周期", class: "operate", scope: "instance" },

  { id: "players.read", label: "查看玩家与封禁", group: "玩家与控制台", class: "read", scope: "instance" },
  { id: "players.kick", label: "踢出玩家", group: "玩家与控制台", class: "operate", scope: "instance" },
  { id: "players.ban", label: "封禁玩家", group: "玩家与控制台", class: "operate", scope: "instance" },
  { id: "players.unban", label: "解除封禁", group: "玩家与控制台", class: "operate", scope: "instance" },
  { id: "players.change_map", label: "切换地图", group: "玩家与控制台", class: "operate", scope: "instance" },
  { id: "players.broadcast", label: "聊天广播", group: "玩家与控制台", class: "operate", scope: "instance" },
  { id: "rcon.execute", label: "执行自定义 RCON", group: "玩家与控制台", class: "operate", scope: "instance", description: "可执行游戏命令的高风险权限" },

  { id: "files.list", label: "浏览文件目录", group: "文件", class: "read", scope: "instance" },
  { id: "files.read", label: "读取文件内容", group: "文件", class: "operate", scope: "instance" },
  { id: "files.write", label: "写入文件", group: "文件", class: "operate", scope: "instance" },
  { id: "files.delete", label: "删除文件", group: "文件", class: "operate", scope: "instance" },
  { id: "files.upload", label: "导入文件", group: "文件", class: "manage", scope: "instance" },

  { id: "packages.read", label: "查看插件与模组", group: "插件与模组", class: "read", scope: "instance" },
  { id: "packages.install", label: "安装插件与模组", group: "插件与模组", class: "manage", scope: "instance" },
  { id: "packages.toggle", label: "启用或停用插件", group: "插件与模组", class: "manage", scope: "instance" },
  { id: "packages.delete", label: "删除插件与模组", group: "插件与模组", class: "manage", scope: "instance" },
  { id: "packages.rollback", label: "回退插件版本", group: "插件与模组", class: "manage", scope: "instance" },

  { id: "backups.read", label: "查看备份", group: "备份与计划", class: "read", scope: "instance" },
  { id: "backups.download", label: "下载备份", group: "备份与计划", class: "operate", scope: "instance" },
  { id: "backups.create", label: "创建备份", group: "备份与计划", class: "operate", scope: "instance" },
  { id: "backups.restore", label: "上传及恢复备份", group: "备份与计划", class: "operate", scope: "instance", description: "会替换实例数据" },
  { id: "schedules.read", label: "查看计划任务", group: "备份与计划", class: "read", scope: "instance" },
  { id: "schedules.manage", label: "管理计划任务", group: "备份与计划", class: "operate", scope: "instance" },
  { id: "logs.read", label: "搜索及下载日志", group: "备份与计划", class: "read", scope: "instance" },

  { id: "maps.read", label: "查看地图库", group: "地图", class: "read", scope: "node" },
  { id: "maps.import", label: "导入地图", group: "地图", class: "manage", scope: "node" },
  { id: "maps.assign", label: "分配或取消地图", group: "地图", class: "manage", scope: "instance" },
  { id: "maps.verify", label: "校验地图", group: "地图", class: "manage", scope: "node" },
  { id: "maps.delete", label: "删除地图", group: "地图", class: "manage", scope: "node" },

  { id: "jobs.read", label: "查看任务", group: "审计与通知", class: "read", scope: "resource" },
  { id: "jobs.details", label: "查看任务正文和结果", group: "审计与通知", class: "operate", scope: "resource" },
  { id: "jobs.cancel", label: "取消任务", group: "审计与通知", class: "operate", scope: "resource" },
  { id: "audit.read", label: "查看审计", group: "审计与通知", class: "read", scope: "resource" },
  { id: "audit.read_global", label: "查看全局审计与来源地址", group: "审计与通知", class: "manage", scope: "global" },
  { id: "notifications.read", label: "查看通知", group: "审计与通知", class: "read", scope: "resource" },
  { id: "notifications.manage", label: "管理通知渠道", group: "审计与通知", class: "manage", scope: "global" },
  { id: "metrics.read", label: "查看监控指标", group: "审计与通知", class: "read", scope: "resource" },
  { id: "root_security", label: "根安全管理", group: "所有者专属", class: "root", scope: "global", description: "团队账号与中心端升级；只属于全局所有者" },
];

const definitions = new Map(permissionCatalog.map((item) => [item.id, item]));
const roleRank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3, owner: 4 };
const classRank: Record<PermissionClass, number> = { read: 1, operate: 2, manage: 3, root: 4 };
type PermissionUser = Pick<User, "role" | "role_bindings">;

export function rolePresetPermissions(role: Role): Permission[] {
  return permissionCatalog.filter((item) => classRank[item.class] <= roleRank[role]).map((item) => item.id);
}

export function permissionAllowedAtScope(permission: Permission, scope: RoleBinding["scope_type"]) {
  const definition = definitions.get(permission);
  if (!definition || scope === "global") return Boolean(definition);
  if (scope === "node") return definition.scope !== "global";
  return definition.scope === "instance" || definition.scope === "resource";
}

function roleAllows(role: Role, permission: Permission) {
  const definition = definitions.get(permission);
  return Boolean(definition && classRank[definition.class] <= roleRank[role]);
}

function bindingAllows(binding: RoleBinding, permission: Permission) {
  return binding.permissions?.length ? binding.permissions.includes(permission) : roleAllows(binding.role, permission);
}

export function hasGlobalPermission(user: PermissionUser | null | undefined, permission: Permission) {
  if (!user) return false;
  const bindings = user.role_bindings || [];
  if (!bindings.length) return roleAllows(user.role, permission);
  return bindings.some((binding) => binding.scope_type === "global" && bindingAllows(binding, permission));
}

export function hasNodePermission(user: PermissionUser | null | undefined, nodeID: string, permission: Permission) {
  if (!user) return false;
  const bindings = user.role_bindings || [];
  if (!bindings.length) return roleAllows(user.role, permission);
  return bindings.some((binding) => (binding.scope_type === "global" || (binding.scope_type === "node" && binding.node_id === nodeID)) && bindingAllows(binding, permission));
}

export function hasInstancePermission(user: PermissionUser | null | undefined, instance: Pick<InstanceRecord, "id" | "node_id">, permission: Permission) {
  if (!user) return false;
  const bindings = user.role_bindings || [];
  if (!bindings.length) return roleAllows(user.role, permission);
  return bindings.some((binding) => {
    const matches = binding.scope_type === "global"
      || (binding.scope_type === "node" && binding.node_id === instance.node_id)
      || (binding.scope_type === "instance" && binding.instance_id === instance.id);
    return matches && bindingAllows(binding, permission);
  });
}

export function hasRootSecurityPermission(user: PermissionUser | null | undefined) {
  return hasGlobalPermission(user, "root_security");
}

export function hasAnyPermission(user: PermissionUser | null | undefined, permission: Permission) {
  if (!user) return false;
  const bindings = user.role_bindings || [];
  if (!bindings.length) return roleAllows(user.role, permission);
  return bindings.some((binding) => bindingAllows(binding, permission));
}

export function tokenScopesForUser(user: PermissionUser | null | undefined): Array<"read" | "operate" | "manage"> {
  if (!user) return ["read"];
  const bindings = user.role_bindings || [];
  const classes = bindings.length
    ? bindings.flatMap((binding) => binding.permissions?.length
      ? binding.permissions.map((permission) => definitions.get(permission)?.class).filter(Boolean)
      : rolePresetPermissions(binding.role).map((permission) => definitions.get(permission)?.class).filter(Boolean))
    : rolePresetPermissions(user.role).map((permission) => definitions.get(permission)?.class).filter(Boolean);
  const highest = classes.reduce((rank, value) => Math.max(rank, value ? classRank[value] : 0), 1);
  return highest >= 3 ? ["read", "operate", "manage"] : highest >= 2 ? ["read", "operate"] : ["read"];
}
