package domain

import "time"

type Role string

const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

type NodeStatus string

const (
	NodePending NodeStatus = "pending"
	NodeOnline  NodeStatus = "online"
	NodeOffline NodeStatus = "offline"
	NodeRevoked NodeStatus = "revoked"
)

type InstanceStatus string

const (
	InstanceInstalling InstanceStatus = "installing"
	InstanceStopped    InstanceStatus = "stopped"
	InstanceStarting   InstanceStatus = "starting"
	InstanceRunning    InstanceStatus = "running"
	InstanceStopping   InstanceStatus = "stopping"
	InstanceUpdating   InstanceStatus = "updating"
	InstanceDegraded   InstanceStatus = "degraded"
	InstanceExternal   InstanceStatus = "external"
)

type JobStatus string

const (
	JobQueued     JobStatus = "queued"
	JobDispatched JobStatus = "dispatched"
	JobRunning    JobStatus = "running"
	JobSucceeded  JobStatus = "succeeded"
	JobFailed     JobStatus = "failed"
	JobCanceled   JobStatus = "canceled"
)

type User struct {
	ID             string        `json:"id"`
	Username       string        `json:"username"`
	DisplayName    string        `json:"display_name"`
	Role           Role          `json:"role"`
	TOTPEnabled    bool          `json:"totp_enabled"`
	CreatedAt      time.Time     `json:"created_at"`
	LastLoginAt    *time.Time    `json:"last_login_at,omitempty"`
	Disabled       bool          `json:"disabled"`
	BootstrapOwner bool          `json:"bootstrap_owner,omitempty"`
	RoleBindings   []RoleBinding `json:"role_bindings,omitempty"`
}

type RoleBinding struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Role       Role      `json:"role"`
	ScopeType  string    `json:"scope_type"`
	NodeID     string    `json:"node_id,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	CreatedBy  string    `json:"created_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Node struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Status        NodeStatus `json:"status"`
	OS            string     `json:"os"`
	Arch          string     `json:"arch"`
	AgentVersion  string     `json:"agent_version"`
	Address       string     `json:"address,omitempty"`
	PublicAddress string     `json:"public_address,omitempty"`
	InstanceRoot  string     `json:"instance_root,omitempty"`
	CPUPercent    float64    `json:"cpu_percent"`
	MemoryUsed    uint64     `json:"memory_used"`
	MemoryTotal   uint64     `json:"memory_total"`
	DiskUsed      uint64     `json:"disk_used"`
	DiskTotal     uint64     `json:"disk_total"`
	NetworkRx     uint64     `json:"network_rx"`
	NetworkTx     uint64     `json:"network_tx"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type InstanceConfig struct {
	Hostname        string            `json:"hostname"`
	ServerPassword  string            `json:"server_password,omitempty"`
	Mode            string            `json:"mode"`
	Difficulty      string            `json:"difficulty"`
	MaxPlayers      int               `json:"max_players"`
	StartMap        string            `json:"start_map"`
	Region          string            `json:"region"`
	SteamGroupID    string            `json:"steam_group_id,omitempty"`
	PublicStatus    bool              `json:"public_status"`
	ExtraArgs       []string          `json:"extra_args,omitempty"`
	CVars           map[string]string `json:"cvars,omitempty"`
	AutoRestart     bool              `json:"auto_restart"`
	MaintenanceCron string            `json:"maintenance_cron,omitempty"`
}

type Instance struct {
	ID                string         `json:"id"`
	NodeID            string         `json:"node_id"`
	NodePublicAddress string         `json:"node_public_address,omitempty"`
	Name              string         `json:"name"`
	Status            InstanceStatus `json:"status"`
	Path              string         `json:"path,omitempty"`
	Host              string         `json:"host"`
	Port              int            `json:"port"`
	TVPort            int            `json:"tv_port,omitempty"`
	PID               int            `json:"pid,omitempty"`
	Map               string         `json:"map,omitempty"`
	Mode              string         `json:"mode,omitempty"`
	Players           int            `json:"players"`
	MaxPlayers        int            `json:"max_players"`
	BuildID           string         `json:"build_id,omitempty"`
	UpdateAvailable   bool           `json:"update_available"`
	AvailableBuild    string         `json:"available_build,omitempty"`
	UpdateCheckedAt   *time.Time     `json:"last_update_checked_at,omitempty"`
	Version           int            `json:"version"`
	Config            InstanceConfig `json:"config"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type Job struct {
	ID             string         `json:"id"`
	NodeID         string         `json:"node_id"`
	InstanceID     string         `json:"instance_id,omitempty"`
	Type           string         `json:"type"`
	Status         JobStatus      `json:"status"`
	Progress       int            `json:"progress"`
	Message        string         `json:"message,omitempty"`
	Payload        map[string]any `json:"payload,omitempty"`
	Result         map[string]any `json:"result,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	RequestedBy    string         `json:"requested_by,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	StartedAt      *time.Time     `json:"started_at,omitempty"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty"`
}

// QuarantineEntry is the control-plane tombstone for an instance directory
// moved into an Agent-owned quarantine directory. EntryName is a safe basename,
// never an absolute node path.
type QuarantineEntry struct {
	ID                 string     `json:"id"`
	NodeID             string     `json:"node_id"`
	SourceJobID        string     `json:"source_job_id"`
	OriginalInstanceID string     `json:"original_instance_id"`
	InstanceName       string     `json:"instance_name"`
	RootIndex          int        `json:"root_index"`
	EntryName          string     `json:"entry_name"`
	QuarantinedAt      time.Time  `json:"quarantined_at"`
	RetentionExpiresAt time.Time  `json:"retention_expires_at"`
	PurgeJobID         string     `json:"purge_job_id,omitempty"`
	PurgedAt           *time.Time `json:"purged_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

type MetricSample struct {
	NodeID      string    `json:"node_id"`
	InstanceID  string    `json:"instance_id,omitempty"`
	CPUPercent  float64   `json:"cpu_percent"`
	MemoryUsed  uint64    `json:"memory_used"`
	MemoryTotal uint64    `json:"memory_total,omitempty"`
	DiskUsed    uint64    `json:"disk_used,omitempty"`
	DiskTotal   uint64    `json:"disk_total,omitempty"`
	NetworkRx   uint64    `json:"network_rx,omitempty"`
	NetworkTx   uint64    `json:"network_tx,omitempty"`
	Players     int       `json:"players,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
}

type AuditEvent struct {
	ID           string         `json:"id"`
	ActorID      string         `json:"actor_id,omitempty"`
	ActorName    string         `json:"actor_name"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id,omitempty"`
	RemoteIP     string         `json:"remote_ip,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

type PublicServer struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Status     InstanceStatus `json:"status"`
	Address    string         `json:"address"`
	Map        string         `json:"map,omitempty"`
	Mode       string         `json:"mode,omitempty"`
	Players    int            `json:"players"`
	MaxPlayers int            `json:"max_players"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
}

type Player struct {
	Name             string `json:"name"`
	SteamID          string `json:"steam_id"`
	Score            int    `json:"score"`
	Ping             int    `json:"ping"`
	ConnectedSeconds int64  `json:"connected_seconds"`
	Address          string `json:"address,omitempty"`
}

type PlayerBan struct {
	ID         string     `json:"id"`
	InstanceID string     `json:"instance_id"`
	SteamID    string     `json:"steam_id,omitempty"`
	IPAddress  string     `json:"ip_address,omitempty"`
	Reason     string     `json:"reason"`
	CreatedBy  string     `json:"created_by"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type AddonPackage struct {
	ID          string    `json:"id"`
	InstanceID  string    `json:"instance_id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Version     string    `json:"version,omitempty"`
	Source      string    `json:"source"`
	SourceID    string    `json:"source_id,omitempty"`
	SHA256      string    `json:"sha256"`
	Path        string    `json:"path,omitempty"`
	Enabled     bool      `json:"enabled"`
	Risk        string    `json:"risk"`
	InstalledBy string    `json:"installed_by,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

type AddonPackageVersion struct {
	ID          string    `json:"id"`
	PackageID   string    `json:"package_id"`
	InstanceID  string    `json:"instance_id"`
	Version     string    `json:"version,omitempty"`
	SHA256      string    `json:"sha256"`
	Size        uint64    `json:"size"`
	TargetPath  string    `json:"target_path"`
	StoragePath string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

type Backup struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instance_id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Path       string    `json:"path,omitempty"`
	Platform   string    `json:"platform"`
	Size       uint64    `json:"size"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"created_at"`
}

// BackupUpload is a resumable, control-plane-tracked transfer whose bytes are
// staged on the target Agent. Paths and encrypted chunk bodies are deliberately
// absent from the public representation.
type BackupUpload struct {
	ID             string    `json:"id"`
	InstanceID     string    `json:"instance_id"`
	Name           string    `json:"name"`
	Platform       string    `json:"platform"`
	Size           uint64    `json:"size"`
	SHA256         string    `json:"sha256"`
	ReceivedSize   uint64    `json:"received_size"`
	Status         string    `json:"status"`
	LastChunkJobID string    `json:"last_chunk_job_id,omitempty"`
	RestoreJobID   string    `json:"restore_job_id,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Schedule struct {
	ID            string         `json:"id"`
	InstanceID    string         `json:"instance_id"`
	Name          string         `json:"name"`
	Cron          string         `json:"cron"`
	Timezone      string         `json:"timezone"`
	Action        string         `json:"action"`
	Payload       map[string]any `json:"payload,omitempty"`
	Enabled       bool           `json:"enabled"`
	MisfirePolicy string         `json:"misfire_policy"`
	LastRunAt     *time.Time     `json:"last_run_at,omitempty"`
	NextRunAt     *time.Time     `json:"next_run_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

type FileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Kind     string    `json:"kind"`
	Size     uint64    `json:"size"`
	Modified time.Time `json:"modified"`
	Editable bool      `json:"editable"`
}

type Notification struct {
	ID        string         `json:"id"`
	Severity  string         `json:"severity"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Message   string         `json:"message"`
	Resource  string         `json:"resource,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	ReadAt    *time.Time     `json:"read_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}
