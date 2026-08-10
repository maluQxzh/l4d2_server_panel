export type Role = "owner" | "admin" | "operator" | "viewer";
export type Permission =
  | "nodes.read" | "nodes.enroll" | "nodes.update" | "nodes.credentials.rotate" | "nodes.discover"
  | "nodes.quarantine.read" | "nodes.quarantine.purge" | "nodes.delete"
  | "environments.read" | "environments.manage"
  | "instances.read" | "instances.create" | "instances.configure" | "instances.delete"
  | "instances.start" | "instances.stop" | "instances.restart"
  | "players.read" | "players.kick" | "players.ban" | "players.unban" | "players.change_map" | "players.broadcast"
  | "rcon.execute" | "files.list" | "files.read" | "files.write" | "files.delete"
  | "packages.read" | "packages.install" | "packages.toggle" | "packages.delete" | "packages.rollback"
  | "backups.read" | "backups.download" | "backups.create" | "backups.restore"
  | "schedules.read" | "schedules.manage" | "logs.read"
  | "maps.read" | "maps.import" | "maps.assign" | "maps.verify" | "maps.delete"
  | "jobs.read" | "jobs.details" | "jobs.cancel" | "audit.read" | "audit.read_global"
  | "notifications.read" | "notifications.manage" | "metrics.read" | "root_security";
export type NodeStatus = "pending" | "online" | "offline" | "revoked";
export type InstanceStatus =
  | "installing"
  | "stopped"
  | "starting"
  | "running"
  | "stopping"
  | "updating"
  | "degraded"
  | "external";
export type JobStatus = "queued" | "dispatched" | "running" | "succeeded" | "failed" | "canceled";

export type SystemUpdatePhase = "validating" | "stopping" | "backing_up" | "installing" | "migrating" | "starting" | "health_check" | "rolling_back";

export interface SystemUpdateRecord {
  job_id: string;
  status: "queued" | "running" | "succeeded" | "failed" | "canceled";
  phase: SystemUpdatePhase;
  current_version: string;
  target_version?: string;
  package_sha256?: string;
  upload_bytes?: number;
  rollback?: "not_required" | "succeeded" | "failed";
  error?: string;
  created_at: string;
  updated_at: string;
  completed_at?: string;
}

export interface SystemUpdateOverview {
  supported: boolean;
  current_version: string;
  deployment?: "windows-portable" | "docker-compose";
  package_format?: "zip" | "tar.gz";
  max_package_bytes?: number;
  active?: SystemUpdateRecord;
  history: SystemUpdateRecord[];
  reason?: string;
}

export interface User {
  id: string;
  username: string;
  display_name: string;
  role: Role;
  totp_enabled: boolean;
  created_at: string;
  last_login_at?: string;
  disabled?: boolean;
  bootstrap_owner?: boolean;
  account_state: "pending_activation" | "active";
  role_bindings?: RoleBinding[];
}

export interface RoleBinding {
  id?: string;
  user_id?: string;
  role: Role;
  permissions?: Permission[];
  scope_type: "global" | "node" | "instance";
  node_id?: string;
  instance_id?: string;
  created_at?: string;
}

export interface NodeRecord {
  id: string;
  name: string;
  status: NodeStatus;
  os: string;
  arch: string;
  agent_version: string;
  address?: string;
  public_address?: string;
  instance_root?: string;
  cpu_percent: number;
  memory_used: number;
  memory_total: number;
  disk_used: number;
  disk_total: number;
  network_rx: number;
  network_tx: number;
  last_seen_at?: string;
  created_at: string;
}

export interface RuntimeEnvironmentRecord {
  id: string;
  node_id: string;
  node_name?: string;
  node_status: NodeStatus;
  agent_supported: boolean;
  system_dependencies: Record<string, unknown>;
  steamcmd_source: "unconfigured" | "managed" | "custom";
  steamcmd_path?: string;
  steamcmd_status: "unconfigured" | "checking" | "ready" | "error";
  steamcmd_version?: string;
  last_error?: string;
  last_inspected_at?: string;
  created_at: string;
  updated_at: string;
}

export interface GameInstallationRecord {
  id: string;
  environment_id: string;
  node_id: string;
  path: string;
  app_id: 222860;
  status: "pending" | "installing" | "validating" | "updating" | "ready" | "degraded";
  build_id?: string;
  last_error?: string;
  bound_instance_id?: string;
  last_checked_at?: string;
  created_at: string;
  updated_at: string;
}

export interface InstallationCandidate {
  root_index: number;
  path: string;
  name: string;
  executable: string;
  has_gameinfo: boolean;
  has_appmanifest: boolean;
  managed: boolean;
  likely_running: boolean;
  pid?: number;
  evidence: string[];
}

export interface NodeDiscoveryResult {
  candidates: InstallationCandidate[];
  scanned_directories: number;
  skipped_links: number;
  truncated: boolean;
}

export interface QuarantineEntry {
  id: string;
  node_id: string;
  source_job_id: string;
  original_instance_id: string;
  instance_name: string;
  root_index: number;
  entry_name: string;
  quarantined_at: string;
  retention_expires_at: string;
  purge_job_id?: string;
  purged_at?: string;
  created_at: string;
}

export interface BackupUploadRecord {
  id: string;
  instance_id: string;
  name: string;
  platform: "windows/amd64" | "linux/amd64";
  size: number;
  sha256: string;
  received_size: number;
  status: string;
  last_chunk_job_id?: string;
  restore_job_id?: string;
  expires_at: string;
  created_at: string;
  updated_at: string;
}

export interface InstanceConfig {
  hostname: string;
  mode: string;
  difficulty: string;
  max_players: number;
  start_map: string;
  region?: string;
  steam_group_id?: string;
  public_status: boolean;
  extra_args?: string[];
  cvars?: Record<string, string>;
  auto_restart: boolean;
}

export interface InstanceRecord {
  id: string;
  node_id: string;
  node_name: string;
  node_status: NodeStatus;
  game_installation_id?: string;
  node_public_address?: string;
  name: string;
  status: InstanceStatus;
  path?: string;
  host: string;
  port: number;
  tv_port?: number;
  pid?: number;
  map?: string;
  mode?: string;
  players: number;
  max_players: number;
  build_id?: string;
  update_available: boolean;
  available_build?: string;
  last_update_checked_at?: string;
  rcon_configured: boolean;
  version: number;
  config: InstanceConfig;
  started_at?: string;
  created_at: string;
  updated_at: string;
}

export type MapHealth = "healthy" | "degraded" | "broken" | "deleting";

export interface MapArtifactRecord {
  id: string;
  ordinal: number;
  sha256: string;
  size: number;
}

export interface MapBSPRecord {
  virtual_path: string;
  start_name?: string;
  dictionary_present: boolean;
}

export interface MapChapterRecord {
  ordinal: number;
  bsp_name: string;
  display_name: string;
  image?: string;
}

export interface MapModeRecord {
  name: string;
  chapters: MapChapterRecord[];
}

export interface MapWarningRecord {
  code: string;
  message: string;
  virtual_path?: string;
}

export interface MapReplicaRecord {
  filesystem_id: string;
  health: "healthy" | "missing" | "corrupt";
  verified_at?: string;
}

export interface MapAssignmentRecord {
  map_id: string;
  instance_id: string;
  restart_required: boolean;
  created_at: string;
  updated_at: string;
}

export interface MapRecord {
  id: string;
  node_id: string;
  name: string;
  author?: string;
  version?: string;
  description?: string;
  poster_url?: string;
  health: MapHealth;
  aggregate_sha256: string;
  total_size: number;
  source_type: "qq_flash";
  source_name?: string;
  removed_files: number;
  removed_bytes: number;
  artifacts?: MapArtifactRecord[];
  bsps?: MapBSPRecord[];
  modes?: MapModeRecord[];
  warnings?: MapWarningRecord[];
  replicas?: MapReplicaRecord[];
  assignments?: MapAssignmentRecord[];
  created_by?: string;
  created_by_display_name?: string;
  created_at: string;
  updated_at: string;
}

export interface Job {
  id: string;
  node_id: string;
	instance_id?: string;
  game_installation_id?: string;
  type: string;
  display_type?: "install_mod" | "install_plugin";
  status: JobStatus;
  progress: number;
  message?: string;
  payload?: Record<string, unknown>;
  result?: Record<string, unknown>;
  requested_by?: string;
  requested_by_display_name?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface JobBackgroundSummary {
  targets: number;
  active: number;
  failed: number;
  latest_at?: string;
}

export interface JobListResponse extends ListResponse<Job> {
  background_summary?: JobBackgroundSummary;
}

export interface AuditEvent {
  id: string;
  actor_id?: string;
  actor_name: string;
  action: string;
  resource_type: string;
  resource_id?: string;
  remote_ip?: string;
  metadata?: Record<string, unknown>;
  created_at: string;
}

export interface PublicServer {
  id: string;
  name: string;
  status: InstanceStatus;
  address: string;
  map?: string;
  mode?: string;
  players: number;
  max_players: number;
  started_at?: string;
}

export interface Player {
  id?: string;
  name: string;
  steam_id?: string;
  score?: number;
  ping?: number;
  connected_at?: string;
}

export interface PlayerBan {
  id: string;
  instance_id: string;
  steam_id: string;
  reason: string;
  created_by?: string;
  expires_at?: string;
  created_at: string;
}

export interface PackageRecord {
  id: string;
  name: string;
  kind: "vpk" | "plugin" | "smx" | "native" | "sourcemod" | "metamod" | "workshop";
  version?: string;
  enabled: boolean;
  source?: string;
  sha256?: string;
  path?: string;
  installed_at?: string;
}

export interface PackageVersionRecord {
  id: string;
  package_id: string;
  instance_id: string;
  version?: string;
  sha256: string;
  size: number;
  target_path: string;
  created_at: string;
}

export interface BackupRecord {
  id: string;
  name: string;
  size: number;
  platform?: string;
  checksum?: string;
  created_at: string;
}

export interface ScheduleRecord {
  id: string;
  name: string;
  cron: string;
  action: string;
  enabled: boolean;
  next_run_at?: string;
}

export interface NotificationRecord {
  id: string;
  level: "info" | "warning" | "critical";
  title: string;
  message: string;
  read: boolean;
  created_at: string;
}

export interface APITokenRecord {
  id: string;
  user_id: string;
  name: string;
  scopes: Array<"read" | "operate" | "manage">;
  node_id?: string;
  instance_id?: string;
  expires_at?: string;
  revoked_at?: string;
  last_used_at?: string;
  created_at: string;
}

export interface ManagedFileContent {
  path: string;
  content?: string;
  content_base64?: string;
  size?: number;
  sha256?: string;
  fresh_at?: string;
  refresh_job_id?: string;
  job_id?: string;
  status?: "loading";
}

export interface NotificationChannelRecord {
  id: string;
  name: string;
  kind: "smtp" | "webhook";
  enabled: boolean;
  config: Record<string, unknown>;
  has_secret: boolean;
  created_at: string;
  updated_at: string;
}

export interface NotificationDeliveryRecord {
  id: string;
  notification_id: string;
  channel_id: string;
  channel_name: string;
  status: "pending" | "delivering" | "succeeded" | "failed" | "skipped";
  attempt_count: number;
  next_attempt_at: string;
  last_error?: string;
  response_code?: number;
  delivered_at?: string;
  updated_at: string;
}

export interface MetricPoint {
  node_id: string;
  instance_id?: string;
  collected_at: string;
  cpu_percent: number;
  memory_used: number;
  memory_total: number;
  disk_used: number;
  disk_total: number;
  network_rx: number;
  network_tx: number;
  players: number;
  sample_count: number;
}

export interface LogSearchEntry {
  at?: string;
  stream: string;
  message: string;
}

export interface TransferChunk {
  blob: Blob;
  offset: number;
  nextOffset: number;
  totalSize: number;
  eof: boolean;
  fileSha256: string;
  chunkSha256: string;
}

export interface RealtimeEvent {
  id: number | string;
  type: string;
  resource_type?: string;
  resource_id?: string;
  data?: Record<string, unknown>;
  created_at: string;
}

export interface ListResponse<T> {
  items: T[];
  total?: number;
  next_cursor?: string;
}
