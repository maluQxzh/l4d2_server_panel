const byteUnits = ["B", "KB", "MB", "GB", "TB"];

export function bytes(value = 0) {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), byteUnits.length - 1);
  return `${(value / 1024 ** index).toFixed(index > 1 ? 1 : 0)} ${byteUnits[index]}`;
}

export function percent(used = 0, total = 0) {
  if (!total) return 0;
  return Math.min(100, Math.max(0, Math.round((used / total) * 100)));
}

export function dateTime(value?: string) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return "—";
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  }).format(date);
}

export function relativeTime(value?: string) {
  if (!value) return "从未";
  const delta = new Date(value).getTime() - Date.now();
  if (!Number.isFinite(delta)) return "未知";
  const minutes = Math.round(delta / 60_000);
  if (Math.abs(minutes) < 1) return "刚刚";
  if (Math.abs(minutes) < 60) return `${Math.abs(minutes)} 分钟${minutes < 0 ? "前" : "后"}`;
  const hours = Math.round(minutes / 60);
  if (Math.abs(hours) < 24) return `${Math.abs(hours)} 小时${hours < 0 ? "前" : "后"}`;
  const days = Math.round(hours / 24);
  return `${Math.abs(days)} 天${days < 0 ? "前" : "后"}`;
}

export function duration(value?: string) {
  if (!value) return "—";
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000));
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  return days ? `${days} 天 ${hours} 小时` : hours ? `${hours} 小时 ${minutes} 分钟` : `${minutes} 分钟`;
}

export const statusLabels: Record<string, string> = {
  online: "在线",
  offline: "离线",
  pending: "待接入",
  revoked: "已吊销",
  running: "运行中",
  stopped: "已停止",
  starting: "启动中",
  stopping: "停止中",
  installing: "安装中",
  updating: "更新中",
  degraded: "异常",
  external: "外部进程",
  queued: "排队中",
  dispatched: "已下发",
  succeeded: "已完成",
  failed: "失败",
  canceled: "已取消",
  owner: "所有者",
  admin: "管理员",
  operator: "操作员",
  viewer: "观察者",
  disabled: "已停用",
};

export const jobLabels: Record<string, string> = {
  create_instance: "创建实例",
  install: "安装游戏",
  start: "启动实例",
  stop: "停止实例",
  restart: "重启实例",
  update: "更新游戏",
  validate: "校验文件",
  backup: "创建备份",
  restore: "恢复备份",
  rcon: "执行 RCON",
  install_package: "安装模组",
  install_mod: "安装模组",
  qq_flash_import: "QQ闪传下载",
  toggle_package: "启停模组",
  rollback_package: "回退模组版本",
  import: "导入实例",
  reinstall: "重新安装",
  configure: "应用配置",
  delete: "隔离实例",
  detach: "解除管理",
  check_update: "检查更新",
  list: "读取文件列表",
  file: "文件操作",
  "a2s.info": "查询服务器状态",
  "a2s.players": "查询 A2S 玩家",
  "rcon.players": "查询 RCON 玩家",
  "logs.search": "检索节点日志",
  "logs.chunk": "读取日志分块",
  "backup.chunk": "读取备份分块",
  "backup.upload.abort": "清理上传暂存",
  discover_instances: "扫描现有安装",
};
