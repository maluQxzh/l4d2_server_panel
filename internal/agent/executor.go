package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type ProgressFunc func(progress int, message string)

type Executor struct {
	config                     *Config
	paths                      *PathGuard
	registry                   *Registry
	processes                  *ProcessManager
	steam                      steamOperations
	steamMu                    sync.RWMutex
	runtimeEnvironment         *runtimeEnvironmentStore
	logger                     *log.Logger
	locks                      *keyedLocker
	officialPackageResolver    func(context.Context, string) (OfficialPackage, error)
	workshopCollectionResolver func(context.Context, string) (bool, []string, error)
	packageDownloader          func(context.Context, string, string, bool) (string, int64, error)
	packageMutationVerifier    func(string, string) error
	transferMu                 sync.Mutex
	transferVerifications      map[string]transferVerification
	telemetryMu                sync.RWMutex
	telemetry                  map[string]instanceTelemetry
}

type instanceTelemetry struct {
	players    int
	observedAt time.Time
}

type steamOperations interface {
	Install(context.Context, string, int, bool, func(int, string)) error
	WorkshopItem(context.Context, string, func(int, string)) ([]string, error)
	PublicBuildID(context.Context, int) (string, error)
	InstalledBuildID(string, int) (string, string, error)
}

func NewExecutor(config *Config, paths *PathGuard, registry *Registry, processes *ProcessManager, logger *log.Logger) *Executor {
	runtimeStore, err := newRuntimeEnvironmentStore(config)
	if err != nil && logger != nil {
		logger.Printf("load runtime environment state: %v", err)
	}
	steamPath := config.SteamCMDPath
	if runtimeStore != nil && runtimeStore.snapshot().SteamCMDPath != "" {
		steamPath = runtimeStore.snapshot().SteamCMDPath
	}
	return &Executor{
		config: config, paths: paths, registry: registry, processes: processes,
		steam: NewSteamCMD(steamPath, config.SteamCMDConcurrency), runtimeEnvironment: runtimeStore, logger: logger, locks: newKeyedLocker(),
		officialPackageResolver: ResolveOfficialPackage, workshopCollectionResolver: ResolveWorkshopCollection,
		packageDownloader: func(ctx context.Context, source, destination string, official bool) (string, int64, error) {
			if official {
				return DownloadOfficialHTTPS(ctx, source, destination)
			}
			return DownloadHTTPS(ctx, source, destination)
		},
	}
}

func (e *Executor) Execute(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	action := normalizeJobType(job.Type)
	if action == "" {
		return nil, errors.New("job type is required")
	}
	if progress == nil {
		progress = func(int, string) {}
	}
	if job.InstanceID != "" {
		if record, ok := e.registry.Get(job.InstanceID); ok && record.ExternalPID > 0 && !externalAgentJobAllowed(action, job.Payload) {
			return nil, errors.New("external instance is read-only until a controlled restart")
		}
	}
	if job.InstanceID != "" && agentJobRequiresInstanceLock(action, job.Payload) {
		release, err := e.locks.acquire(ctx, job.InstanceID)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	progress(1, "preparing operation")
	switch action {
	case "environment.inspect":
		return e.inspectRuntimeEnvironment(ctx, job, progress)
	case "steamcmd.configure":
		return e.configureSteamCMD(ctx, job, "custom", progress)
	case "steamcmd.clear":
		return e.clearSteamCMD(progress)
	case "steamcmd.install", "steamcmd.repair":
		return e.installManagedSteamCMD(ctx, progress)
	case "game.install", "game.import", "game.validate", "game.update", "game.reinstall":
		if action == "game.import" && job.InstanceID != "" {
			return e.importInstance(ctx, job, progress)
		}
		return e.gameInstallationAction(ctx, job, action, progress)
	case "create":
		return e.create(ctx, job, progress)
	case "configure":
		return e.configure(ctx, job, progress)
	case "install":
		return e.install(ctx, job, false, progress)
	case "reinstall":
		if _, registered := e.registry.Get(job.InstanceID); registered {
			return e.update(ctx, job, true, progress)
		}
		return e.install(ctx, job, true, progress)
	case "start":
		return e.start(ctx, job, progress)
	case "stop":
		return e.stop(ctx, job, progress)
	case "restart":
		if record, ok := e.registry.Get(job.InstanceID); ok && record.ExternalPID > 0 {
			return e.restartExternal(ctx, job, record, progress)
		}
		was := processStatusActive(e.processes.State(job.InstanceID).Status)
		if _, err := e.stop(ctx, job, progress); err != nil {
			return nil, err
		}
		if !was {
			progress(100, "instance was already stopped")
			return map[string]any{"status": "stopped"}, nil
		}
		return e.start(ctx, job, progress)
	case "update":
		return e.update(ctx, job, false, progress)
	case "validate":
		return e.update(ctx, job, true, progress)
	case "import":
		return e.importInstance(ctx, job, progress)
	case "backup":
		return e.backup(ctx, job, progress)
	case "restore":
		return e.restore(ctx, job, progress)
	case "file":
		return e.file(ctx, job, progress)
	case "list":
		return e.list(ctx, job, progress)
	case "rcon":
		return e.rcon(ctx, job, progress)
	case "rcon.players":
		return e.rconPlayers(ctx, job, progress)
	case "logs.search":
		return e.searchLogs(ctx, job, progress)
	case "logs.chunk":
		return e.logChunk(ctx, job, progress)
	case "backup.chunk":
		return e.backupChunk(ctx, job, progress)
	case "backup.upload.chunk":
		return e.backupUploadChunk(ctx, job, progress)
	case "backup.upload.abort":
		return e.abortBackupUpload(ctx, job, progress)
	case "kick_player", "ban_player", "unban_player", "change_map":
		return e.playerAction(ctx, job, action, progress)
	case "a2s.info":
		return e.a2sInfo(ctx, job)
	case "a2s.players":
		return e.a2sPlayers(ctx, job)
	case "check_update":
		return e.checkUpdate(ctx, job, progress)
	case "discover_instances":
		return e.discoverInstances(ctx, job, progress)
	case "purge_quarantine":
		return e.purgeQuarantine(job, progress)
	case "delete", "detach":
		return e.deleteInstance(ctx, job, action == "detach", progress)
	case "install_mod", "install_package":
		return e.installMod(ctx, job, progress)
	case "toggle_package":
		return e.togglePackage(ctx, job, progress)
	case "rollback_package":
		return e.rollbackPackage(ctx, job, progress)
	default:
		return nil, fmt.Errorf("unsupported job type %q", job.Type)
	}
}

func (e *Executor) steamClient() steamOperations {
	e.steamMu.RLock()
	defer e.steamMu.RUnlock()
	return e.steam
}

func (e *Executor) replaceSteamClient(path string) {
	e.steamMu.Lock()
	e.steam = NewSteamCMD(path, e.config.SteamCMDConcurrency)
	e.steamMu.Unlock()
}

func agentJobRequiresInstanceLock(action string, payload map[string]any) bool {
	// check_update reads Steam app metadata and the same appmanifest/files that
	// update replaces. It therefore participates in the instance lock even
	// though it does not mutate files. A2S/RCON status and bounded file/log
	// reads remain concurrent with lifecycle work.
	readOnly := action == "discover_instances" || action == "list" || action == "a2s.info" || action == "a2s.players" || action == "rcon.players" || action == "logs.search" || action == "logs.chunk" || action == "backup.chunk" || action == "backup.upload.abort" || (action == "file" && payloadString(payload, "operation") == "read")
	return !readOnly
}

func externalAgentJobAllowed(action string, payload map[string]any) bool {
	switch action {
	case "restart", "a2s.info", "a2s.players", "rcon.players", "list", "logs.search", "logs.chunk", "backup", "backup.chunk", "backup.upload.chunk", "backup.upload.abort", "check_update", "detach":
		return true
	case "file":
		operation := strings.ToLower(strings.TrimSpace(payloadString(payload, "operation")))
		return operation == "" || operation == "read"
	case "delete":
		return strings.EqualFold(payloadString(payload, "mode"), "detach")
	default:
		return false
	}
}

func normalizeJobType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", ".")
	value = strings.TrimPrefix(value, "instance.")
	switch value {
	case "file.list", "files.list":
		return "list"
	case "file.manage", "files":
		return "file"
	case "health", "health.check":
		return "a2s.info"
	case "install.mod", "mod.install":
		return "install_mod"
	case "package.install", "install.package":
		return "install_package"
	case "package.toggle", "toggle.addon", "toggle.package":
		return "toggle_package"
	case "package.rollback", "rollback.package":
		return "rollback_package"
	case "player.kick", "kick", "kick.player":
		return "kick_player"
	case "player.ban", "ban", "ban.player":
		return "ban_player"
	case "player.unban", "unban", "unban.player":
		return "unban_player"
	case "map.change", "changelevel", "change.map":
		return "change_map"
	case "apply.config", "config.apply":
		return "configure"
	case "players", "query.players", "players.query":
		return "rcon.players"
	case "check.update", "update.check":
		return "check_update"
	case "discover", "discover.instances", "instances.discover":
		return "discover_instances"
	case "quarantine.purge", "purge.quarantine":
		return "purge_quarantine"
	}
	return value
}

type operationPayload struct {
	Operation            string            `json:"operation,omitempty"`
	Instance             domain.Instance   `json:"instance,omitempty"`
	RootIndex            int               `json:"root_index,omitempty"`
	Path                 string            `json:"path,omitempty"`
	Name                 string            `json:"name,omitempty"`
	Executable           string            `json:"executable,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	Env                  map[string]string `json:"env,omitempty"`
	AutoRestart          *bool             `json:"auto_restart,omitempty"`
	Port                 int               `json:"port,omitempty"`
	TVPort               int               `json:"tv_port,omitempty"`
	MaxPlayers           int               `json:"max_players,omitempty"`
	Map                  string            `json:"map,omitempty"`
	Mode                 string            `json:"mode,omitempty"`
	ConfigFile           string            `json:"config_file,omitempty"`
	ExtraArgs            []string          `json:"extra_args,omitempty"`
	Validate             bool              `json:"validate,omitempty"`
	AppID                int               `json:"app_id,omitempty"`
	RCONHost             string            `json:"rcon_host,omitempty"`
	RCONPort             int               `json:"rcon_port,omitempty"`
	RCONPassword         string            `json:"rcon_password,omitempty"`
	ExistingRCONPassword string            `json:"existing_rcon_password,omitempty"`
	ServerPassword       string            `json:"server_password,omitempty"`
	Command              string            `json:"command,omitempty"`
	TimeoutSeconds       int               `json:"timeout_seconds,omitempty"`
	Relative             string            `json:"relative_path,omitempty"`
	Content              string            `json:"content,omitempty"`
	ContentBase64        string            `json:"content_base64,omitempty"`
	Archive              string            `json:"archive,omitempty"`
	SourcePath           string            `json:"source_path,omitempty"`
	Source               string            `json:"source,omitempty"`
	Kind                 string            `json:"kind,omitempty"`
	SourceID             string            `json:"source_id,omitempty"`
	PackagePath          string            `json:"package_path,omitempty"`
	SnapshotPath         string            `json:"snapshot_path,omitempty"`
	RestoreVersion       string            `json:"restore_version,omitempty"`
	Enabled              bool              `json:"enabled,omitempty"`
	RiskConfirmed        bool              `json:"risk_confirmed,omitempty"`
	AcknowledgeRisk      bool              `json:"acknowledge_native_risk,omitempty"`
	SteamID              string            `json:"steam_id,omitempty"`
	Reason               string            `json:"reason,omitempty"`
	DurationMinutes      int               `json:"duration_minutes,omitempty"`
	RetentionDaily       int               `json:"retention_daily,omitempty"`
	RetentionWeekly      int               `json:"retention_weekly,omitempty"`
	Maintenance          bool              `json:"maintenance,omitempty"`
	WarningSeconds       []int             `json:"warning_seconds,omitempty"`
	BroadcastMessage     string            `json:"broadcast_message,omitempty"`
	HealthTimeoutSeconds int               `json:"health_timeout_seconds,omitempty"`
	VerifyStart          bool              `json:"verify_start,omitempty"`
	ExpectedSHA256       string            `json:"expected_sha256,omitempty"`
	ExpectedSize         int64             `json:"expected_size,omitempty"`
	BackupID             string            `json:"backup_id,omitempty"`
	UploadID             string            `json:"upload_id,omitempty"`
	ChunkSHA256          string            `json:"chunk_sha256,omitempty"`
	ExpiresAt            string            `json:"expires_at,omitempty"`
	Keyword              string            `json:"keyword,omitempty"`
	Since                string            `json:"since,omitempty"`
	Until                string            `json:"until,omitempty"`
	Limit                int               `json:"limit,omitempty"`
	Segment              int               `json:"segment,omitempty"`
	Offset               int64             `json:"offset,omitempty"`
	Length               int               `json:"length,omitempty"`
	MaxDepth             int               `json:"max_depth,omitempty"`
	MaxEntries           int               `json:"max_entries,omitempty"`
	QuarantineID         string            `json:"quarantine_entry_id,omitempty"`
	QuarantineEntry      string            `json:"quarantine_entry,omitempty"`
}

func decodeOperation(job domain.Job) (operationPayload, error) {
	var payload operationPayload
	raw, err := json.Marshal(job.Payload)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func (e *Executor) create(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if _, exists := e.registry.Get(job.InstanceID); exists {
		return nil, errors.New("instance is already registered")
	}
	record, path, _, err := e.createRecord(job, payload)
	if err != nil {
		return nil, err
	}
	progress(100, "instance directories created")
	result := map[string]any{"instance_id": record.ID, "path": path, "root_index": record.RootIndex}
	return result, nil
}

func (e *Executor) configure(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	record, configPath, err := e.applyConfiguration(record, payload)
	if err != nil {
		return nil, err
	}
	progress(100, "generated server configuration updated")
	return map[string]any{"instance_id": record.ID, "config_path": configPath, "restart_required": e.processes.State(record.ID).Status == "running"}, nil
}

func (e *Executor) applyConfiguration(record InstanceRecord, payload operationPayload) (InstanceRecord, string, error) {
	if payload.RCONPassword == "" {
		return record, "", errors.New("rcon_password is required to generate configuration")
	}
	config := payload.Instance.Config
	config.ServerPassword = payload.ServerPassword
	configRaw, err := buildServerConfig(config, payload.RCONPassword)
	if err != nil {
		return record, "", err
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return record, "", err
	}
	configRelative := "left4dead2/cfg/l4d2panel.cfg"
	if err := writeGeneratedServerConfig(instancePath, []byte(configRaw)); err != nil {
		return record, "", err
	}
	port := payload.Instance.Port
	if port == 0 {
		port = record.RCONPort
	}
	if port == 0 {
		port = 27015
	}
	maxPlayers := config.MaxPlayers
	if maxPlayers == 0 {
		maxPlayers = 8
	}
	args, err := BuildStartArgs(StartOptions{Port: port, TVPort: payload.Instance.TVPort, MaxPlayers: maxPlayers, Map: config.StartMap, Mode: config.Mode, ConfigFile: "l4d2panel.cfg", ExtraArgs: append(config.ExtraArgs, payload.ExtraArgs...)})
	if err != nil {
		return record, "", err
	}
	host := localGameEndpointHost(payload.Instance.Host, payload.RCONHost)
	tvPort := payload.Instance.TVPort
	if tvPort == 0 {
		tvPort = payload.TVPort
	}
	record.Args, record.AutoRestart, record.RCONHost, record.RCONPort, record.TVPort = args, config.AutoRestart, host, port, tvPort
	if payload.Instance.Name != "" {
		record.Name = payload.Instance.Name
	}
	if err := e.registry.Put(record); err != nil {
		return record, "", err
	}
	return record, configRelative, nil
}

func (e *Executor) createRecord(job domain.Job, payload operationPayload) (InstanceRecord, string, string, error) {
	if job.InstanceID == "" {
		return InstanceRecord{}, "", "", errors.New("instance id is required")
	}
	rootIndex := payload.RootIndex
	relative := strings.TrimSpace(payload.Path)
	if payload.Instance.ID != "" {
		if payload.Instance.Path != "" {
			relative = payload.Instance.Path
		}
		if payload.Instance.Name != "" {
			payload.Name = payload.Instance.Name
		}
	}
	var instancePath string
	var err error
	if relative == "" {
		relative = job.InstanceID
	}
	if filepath.IsAbs(relative) {
		instancePath, err = e.paths.ResolveInstallation(rootIndex, relative)
		relative = instancePath
	} else {
		instancePath, err = e.paths.Resolve(rootIndex, relative)
	}
	if err != nil {
		return InstanceRecord{}, "", "", err
	}
	if entries, readErr := os.ReadDir(instancePath); readErr == nil && len(entries) > 0 {
		return InstanceRecord{}, "", "", errors.New("instance directory is not empty; use import")
	}
	port := payload.Instance.Port
	if port == 0 {
		port = payload.Port
	}
	if port == 0 {
		port = 27015
	}
	tvPort := payload.Instance.TVPort
	if tvPort == 0 {
		tvPort = payload.TVPort
	}
	if err := ProbeLocalPorts(port, tvPort); err != nil {
		return InstanceRecord{}, "", "", err
	}
	for _, dir := range []string{"left4dead2/cfg", "left4dead2/addons"} {
		path, resolveErr := ResolveWithin(instancePath, filepath.FromSlash(dir))
		if resolveErr != nil {
			return InstanceRecord{}, "", "", resolveErr
		}
		if err := os.MkdirAll(path, 0o750); err != nil {
			return InstanceRecord{}, "", "", err
		}
	}
	config := payload.Instance.Config
	config.ServerPassword = payload.ServerPassword
	if config.Hostname == "" {
		config.Hostname = payload.Name
	}
	secret := payload.RCONPassword
	if secret == "" {
		return InstanceRecord{}, "", "", errors.New("rcon_password is required to create an instance")
	}
	configRaw, err := buildServerConfig(config, secret)
	if err != nil {
		return InstanceRecord{}, "", "", err
	}
	if err := writeGeneratedServerConfig(instancePath, []byte(configRaw)); err != nil {
		return InstanceRecord{}, "", "", err
	}
	maxPlayers := config.MaxPlayers
	if maxPlayers == 0 {
		maxPlayers = payload.MaxPlayers
	}
	if maxPlayers == 0 {
		maxPlayers = 8
	}
	startMap := config.StartMap
	if startMap == "" {
		startMap = payload.Map
	}
	mode := config.Mode
	if mode == "" {
		mode = payload.Mode
	}
	args, err := BuildStartArgs(StartOptions{Port: port, TVPort: tvPort, MaxPlayers: maxPlayers, Map: startMap, Mode: mode, ConfigFile: "l4d2panel.cfg", ExtraArgs: append(config.ExtraArgs, payload.ExtraArgs...)})
	if err != nil {
		return InstanceRecord{}, "", "", err
	}
	name := payload.Name
	if name == "" {
		name = payload.Instance.Name
	}
	if name == "" {
		name = job.InstanceID
	}
	host := localGameEndpointHost(payload.Instance.Host, payload.RCONHost)
	record := InstanceRecord{ID: job.InstanceID, Name: name, RootIndex: rootIndex, RelativePath: relative, Executable: defaultServerExecutable(), Args: args, AutoRestart: config.AutoRestart, DesiredStateKnown: true, RCONHost: host, RCONPort: port, TVPort: tvPort}
	if err := e.registry.Put(record); err != nil {
		return InstanceRecord{}, "", "", err
	}
	return record, instancePath, secret, nil
}

func (e *Executor) install(ctx context.Context, job domain.Job, reinstall bool, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		var ignored string
		record, _, ignored, err = e.createRecord(job, payload)
		if err != nil {
			return nil, err
		}
		_ = ignored
	} else if payload.RCONPassword != "" && payload.Instance.ID != "" {
		temporaryConfig := payload.Instance.Config
		temporaryConfig.ServerPassword = payload.ServerPassword
		configRaw, configErr := buildServerConfig(temporaryConfig, payload.RCONPassword)
		if configErr != nil {
			return nil, configErr
		}
		path, _ := e.instancePath(record)
		if err := writeGeneratedServerConfig(path, []byte(configRaw)); err != nil {
			return nil, err
		}
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	appID := payload.AppID
	if appID == 0 {
		appID = L4D2DedicatedAppID
	}
	if appID != L4D2DedicatedAppID {
		return nil, errors.New("only L4D2 dedicated server app 222860 is supported")
	}
	validate := reinstall || payload.Validate
	progress(5, "starting SteamCMD")
	steam := e.steamClient()
	err = steam.Install(ctx, path, appID, validate, func(p int, message string) {
		if p > 0 {
			progress(5+p*9/10, message)
		}
	})
	if err != nil {
		return nil, err
	}
	if payload.VerifyStart {
		progress(90, "L4D2 dedicated server installed; starting verification")
	} else {
		progress(100, "L4D2 dedicated server installed")
	}
	result := map[string]any{"instance_id": record.ID, "path": path, "app_id": appID, "validated": validate}
	if buildID, source, buildErr := steam.InstalledBuildID(path, appID); buildErr == nil {
		result["build_id"] = buildID
		result["build_source"] = source
	}
	if payload.VerifyStart {
		progress(92, "verifying installed server startup")
		started, startErr := e.start(ctx, job, func(int, string) {})
		if startErr != nil {
			_ = e.processes.Stop(job.InstanceID, 5*time.Second)
			return nil, fmt.Errorf("installation startup verification: %w", startErr)
		}
		if _, stopErr := e.stop(ctx, job, func(int, string) {}); stopErr != nil {
			return nil, fmt.Errorf("stop after installation verification: %w", stopErr)
		}
		result["startup_verified"] = true
		result["verification"] = started
		progress(100, "installation and startup verification completed")
	}
	return result, nil
}

func (e *Executor) start(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if payload.Executable != "" || len(payload.Args) > 0 || len(payload.Env) > 0 {
		return nil, errors.New("start jobs cannot override the registered executable, arguments, or environment")
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	if record.ExternalPID > 0 {
		return nil, errors.New("external instance is read-only; use restart to transfer process ownership")
	}
	currentState := e.processes.State(job.InstanceID)
	if currentState.Status == "running" || currentState.Status == "starting" || currentState.Status == "degraded" {
		return nil, errors.New("instance process is already running")
	}
	if payload.Instance.ID != "" && payload.RCONPassword != "" {
		record, _, err = e.applyConfiguration(record, payload)
		if err != nil {
			return nil, err
		}
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	executable := record.Executable
	if executable == "" {
		executable = defaultServerExecutable()
	}
	if err := validateServerExecutable(executable); err != nil {
		return nil, err
	}
	executablePath, err := ResolveWithin(path, executable)
	if err != nil {
		return nil, err
	}
	args := append([]string(nil), record.Args...)
	if err := ValidateStartArgs(args); err != nil {
		return nil, err
	}
	gamePort := record.RCONPort
	tvPort := record.TVPort
	if err := ProbeLocalPorts(gamePort, tvPort); err != nil {
		return nil, err
	}
	autoRestart := record.AutoRestart
	progress(30, "starting server process")
	state, err := e.processes.Start(ProcessSpec{InstanceID: job.InstanceID, Dir: path, Executable: executablePath, Args: args, AutoRestart: autoRestart})
	if err != nil {
		return nil, err
	}
	record.Executable, record.Args, record.AutoRestart = executable, args, autoRestart
	if job.Type == "start" || job.Type == "restart" {
		record.DesiredRunning = true
	}
	if err := e.registry.Put(record); err != nil {
		_ = e.processes.Stop(job.InstanceID, 5*time.Second)
		return nil, fmt.Errorf("persist running state: %w", err)
	}
	health := "not_checked"
	if payload.RCONPassword != "" {
		progress(60, "waiting for A2S and RCON health checks")
		timeout := 30 * time.Second
		if payload.HealthTimeoutSeconds > 0 && payload.HealthTimeoutSeconds <= 300 {
			timeout = time.Duration(payload.HealthTimeoutSeconds) * time.Second
		}
		if err := e.waitForStartHealth(ctx, record, payload, timeout); err != nil {
			e.processes.MarkDegraded(job.InstanceID, err.Error())
			return nil, fmt.Errorf("server started but health checks failed: %w", err)
		}
		health = "healthy"
	}
	progress(100, "server process started")
	return map[string]any{"status": state.Status, "pid": state.PID, "started_at": state.StartedAt, "health": health}, nil
}

func (e *Executor) stop(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	if record.ExternalPID > 0 {
		return nil, errors.New("external instance is read-only; use restart to transfer process ownership")
	}
	if job.Type == "stop" {
		record.DesiredRunning = false
		if err := e.registry.Put(record); err != nil {
			return nil, fmt.Errorf("persist stopped state: %w", err)
		}
	}
	state := e.processes.State(job.InstanceID)
	if state.Status != "running" && state.Status != "starting" && state.Status != "stopping" && state.Status != "degraded" && state.Status != "crash_loop" {
		progress(100, "server is already stopped")
		return map[string]any{"status": "stopped"}, nil
	}
	grace := 20 * time.Second
	if payload.TimeoutSeconds > 0 && payload.TimeoutSeconds <= 300 {
		grace = time.Duration(payload.TimeoutSeconds) * time.Second
	}
	password := payload.RCONPassword
	if payload.Maintenance || len(payload.WarningSeconds) > 0 {
		if password == "" {
			return nil, errors.New("maintenance countdown requires rcon_password")
		}
		if err := e.maintenanceCountdown(ctx, record, payload, progress); err != nil {
			return nil, err
		}
	}
	if password != "" {
		host, port := rconEndpoint(record, payload)
		progress(15, "requesting graceful RCON shutdown")
		_, rconErr := (RCONClient{Timeout: minDuration(grace/2, 8*time.Second)}).Execute(ctx, net.JoinHostPort(host, strconv.Itoa(port)), password, "quit")
		if rconErr != nil {
			e.logger.Printf("instance %s RCON quit failed: %v", job.InstanceID, rconErr)
		} else {
			deadline := time.Now().Add(grace / 2)
			for time.Now().Before(deadline) {
				if e.processes.State(job.InstanceID).Status == "stopped" {
					progress(100, "server stopped through RCON")
					return map[string]any{"status": "stopped", "graceful": true}, nil
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
	}
	progress(60, "stopping process tree")
	if err := e.processes.Stop(job.InstanceID, grace); err != nil {
		return nil, err
	}
	progress(100, "server stopped")
	return map[string]any{"status": "stopped", "graceful": password != ""}, nil
}

func (e *Executor) restartExternal(ctx context.Context, job domain.Job, record InstanceRecord, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if payload.Instance.ID == "" || payload.RCONPassword == "" {
		return nil, errors.New("controlled restart requires current instance configuration and RCON credentials")
	}
	previousPID := record.ExternalPID
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	progress(10, "preparing controlled restart of external server")
	gracefulHandoff := false
	if externalProcessMatches(ctx, record.ExternalPID, path) && payload.ExistingRCONPassword != "" {
		host, port := rconEndpoint(record, payload)
		progress(20, "requesting graceful RCON shutdown")
		_, rconErr := (RCONClient{Timeout: 5 * time.Second}).Execute(ctx, net.JoinHostPort(host, strconv.Itoa(port)), payload.ExistingRCONPassword, "quit")
		if rconErr == nil {
			gracefulHandoff = true
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) && externalProcessMatches(ctx, record.ExternalPID, path) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
			}
		}
	}
	if externalProcessMatches(ctx, record.ExternalPID, path) {
		progress(35, "stopping verified external process tree")
		if err := terminateExternalProcessTree(ctx, record.ExternalPID, path, 12*time.Second); err != nil {
			return nil, err
		}
	}
	if externalPIDStillRunning(ctx, record.ExternalPID) {
		return nil, errors.New("external process is still running or cannot be safely inspected")
	}
	record.ExternalPID = 0
	if err := e.registry.Put(record); err != nil {
		return nil, err
	}
	progress(55, "installing managed configuration")
	if _, _, err := e.applyConfiguration(record, payload); err != nil {
		return nil, err
	}
	started, err := e.start(ctx, job, func(value int, message string) {
		progress(55+value*45/100, message)
	})
	if err != nil {
		return nil, err
	}
	started["adopted_external_process"] = true
	started["previous_pid"] = previousPID
	started["graceful_handoff"] = gracefulHandoff
	return started, nil
}

func (e *Executor) update(ctx context.Context, job domain.Job, validate bool, progress ProgressFunc) (result map[string]any, retErr error) {
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	wasRunning := processStatusActive(e.processes.State(job.InstanceID).Status)
	// Any failure after a previously-running server is stopped must make a
	// best-effort attempt to restore availability, including cancellation and
	// Steam outages. Valve may remove old depots, so this restarts the verified
	// files that remain rather than claiming a binary rollback.
	defer func() {
		if retErr == nil || !wasRunning || processStatusActive(e.processes.State(job.InstanceID).Status) {
			return
		}
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, recoveryErr := e.start(recoveryCtx, job, func(int, string) {}); recoveryErr != nil {
			retErr = fmt.Errorf("%w; automatic availability recovery failed: %v", retErr, recoveryErr)
		}
	}()
	// The maintenance contract intentionally snapshots before broadcasting and
	// stopping, so a backup failure cannot take an otherwise healthy server down.
	backupJob := backupJobWithKind(job, "checkpoint")
	if _, err := e.backup(ctx, backupJob, func(p int, m string) { progress(p/5, m) }); err != nil {
		return nil, fmt.Errorf("pre-update backup: %w", err)
	}
	if wasRunning {
		if _, err := e.stop(ctx, job, func(p int, m string) { progress(20+p/5, m) }); err != nil {
			return nil, err
		}
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	progress(40, "updating with SteamCMD")
	steam := e.steamClient()
	if err := steam.Install(ctx, path, L4D2DedicatedAppID, validate, func(p int, m string) {
		if p > 0 {
			progress(40+p/2, m)
		}
	}); err != nil {
		return nil, err
	}
	result = map[string]any{"updated": true, "validated": validate, "was_running": wasRunning}
	if buildID, source, buildErr := steam.InstalledBuildID(path, L4D2DedicatedAppID); buildErr == nil {
		result["build_id"] = buildID
		result["build_source"] = source
	}
	if wasRunning {
		started, err := e.start(ctx, job, func(p int, m string) { progress(90+p/10, m) })
		if err != nil {
			return nil, fmt.Errorf("updated but failed to restart: %w", err)
		}
		result["restart"] = started
	}
	progress(100, "update completed")
	return result, nil
}

func (e *Executor) checkUpdate(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	progress(20, "querying Steam public branch metadata")
	steam := e.steamClient()
	availableBuild, err := steam.PublicBuildID(ctx, L4D2DedicatedAppID)
	if err != nil {
		return nil, err
	}
	progress(75, "reading installed app manifest")
	installedBuild, source, installedErr := steam.InstalledBuildID(path, L4D2DedicatedAppID)
	result := map[string]any{
		"app_id":          L4D2DedicatedAppID,
		"available":       false,
		"available_build": availableBuild,
		"installed_build": installedBuild,
	}
	if installedErr != nil {
		if errors.Is(installedErr, os.ErrNotExist) {
			result["reason"] = "installed_manifest_not_found"
			progress(100, "public build checked; installed build is unknown")
			return result, nil
		}
		return nil, installedErr
	}
	result["installed_source"] = source
	result["available"] = installedBuild != availableBuild
	progress(100, "game update check completed")
	return result, nil
}

func (e *Executor) discoverInstances(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	progress(5, "scanning configured instance roots")
	result, err := DiscoverInstallations(ctx, e.paths, e.registry, payload.MaxDepth, payload.MaxEntries)
	if err != nil {
		return nil, err
	}
	progress(100, "instance discovery completed")
	return map[string]any{
		"candidates":          result.Candidates,
		"scanned_directories": result.ScannedDirectories,
		"skipped_links":       result.SkippedLinks,
		"truncated":           result.Truncated,
	}, nil
}

func (e *Executor) importInstance(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	requestedPath := firstNonEmpty(payload.Path, payload.Instance.Path)
	if requestedPath == "" {
		return nil, errors.New("import path is required")
	}
	rootIndex := payload.RootIndex
	var path string
	if filepath.IsAbs(requestedPath) {
		path, err = e.paths.ResolveInstallation(rootIndex, requestedPath)
	} else {
		path, err = e.paths.Resolve(rootIndex, requestedPath)
	}
	if err != nil {
		return nil, err
	}
	relative := filepath.Clean(requestedPath)
	if !filepath.IsAbs(requestedPath) {
		relative, err = filepath.Rel(e.paths.Roots()[rootIndex], path)
		if err != nil {
			return nil, err
		}
	}
	bindRegistered := payload.Operation == "bind"
	if bindRegistered {
		if job.InstallationID == "" || e.runtimeEnvironment == nil {
			return nil, errors.New("binding requires a registered game installation")
		}
		registered, ok := e.runtimeEnvironment.snapshot().Installations[job.InstallationID]
		if !ok || !samePath(canonicalProcessPath(registered.Path), canonicalProcessPath(path)) {
			return nil, errors.New("game installation is not registered at the requested path")
		}
	}
	for _, existing := range e.registry.List() {
		if existing.ID == job.InstanceID {
			continue
		}
		existingPath, resolveErr := e.instancePath(existing)
		if resolveErr == nil && samePath(canonicalProcessPath(existingPath), canonicalProcessPath(path)) {
			return nil, errors.New("installation path is already managed by another instance")
		}
	}
	executable := payload.Executable
	if executable == "" {
		executable = defaultServerExecutable()
	}
	if err := validateServerExecutable(executable); err != nil {
		return nil, err
	}
	if !bindRegistered {
		exePath, err := ResolveWithin(path, executable)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(exePath); err != nil || st.IsDir() {
			return nil, errors.New("path does not contain an L4D2 dedicated server executable")
		}
		gameInfo, err := ResolveWithin(path, "left4dead2/gameinfo.txt")
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(gameInfo); err != nil {
			return nil, errors.New("path does not contain left4dead2/gameinfo.txt")
		}
	}
	config := payload.Instance.Config
	args := append([]string(nil), payload.Args...)
	port := payload.Instance.Port
	if port == 0 {
		port = payload.RCONPort
	}
	if port == 0 {
		port = payload.Port
	}
	if port == 0 {
		port = 27015
	}
	if len(args) == 0 {
		maxPlayers := config.MaxPlayers
		if maxPlayers == 0 {
			maxPlayers = payload.MaxPlayers
		}
		if maxPlayers == 0 {
			maxPlayers = 8
		}
		args, err = BuildStartArgs(StartOptions{Port: port, TVPort: payload.Instance.TVPort, MaxPlayers: maxPlayers, Map: firstNonEmpty(config.StartMap, payload.Map), Mode: firstNonEmpty(config.Mode, payload.Mode), ConfigFile: "l4d2panel.cfg", ExtraArgs: append(config.ExtraArgs, payload.ExtraArgs...)})
		if err != nil {
			return nil, err
		}
	} else if err := ValidateStartArgs(args); err != nil {
		return nil, err
	}
	autoRestart := config.AutoRestart
	if payload.AutoRestart != nil {
		autoRestart = *payload.AutoRestart
	}
	host := localGameEndpointHost(payload.Instance.Host, payload.RCONHost)
	name := firstNonEmpty(payload.Name, payload.Instance.Name, job.InstanceID)
	var external discoveredProcess
	likelyRunning := false
	if !bindRegistered {
		external, likelyRunning = installationProcessForDirectory(path, activeSrcdsProcesses(ctx))
	}
	record := InstanceRecord{ID: job.InstanceID, Name: name, RootIndex: rootIndex, RelativePath: relative, Executable: executable, Args: args, AutoRestart: autoRestart, DesiredStateKnown: true, RCONHost: host, RCONPort: port, TVPort: payload.Instance.TVPort}
	if likelyRunning {
		record.ExternalPID = external.PID
	}
	if record.Name == "" {
		record.Name = job.InstanceID
	}
	if err := e.registry.Put(record); err != nil {
		return nil, err
	}
	if job.InstallationID != "" && e.runtimeEnvironment != nil {
		if err := e.runtimeEnvironment.update(func(state *runtimeEnvironmentState) {
			state.Installations[job.InstallationID] = runtimeGameRecord{Path: relative}
		}); err != nil {
			_ = e.registry.Delete(job.InstanceID)
			return nil, err
		}
	}
	// Do not alter files underneath an already-running, externally-owned
	// server. The generated configuration is installed by restartExternal
	// immediately before the Agent takes process ownership.
	if payload.RCONPassword != "" && !likelyRunning {
		record, _, err = e.applyConfiguration(record, payload)
		if err != nil {
			_ = e.registry.Delete(job.InstanceID)
			return nil, err
		}
	}
	if bindRegistered {
		progress(100, "registered game installation bound")
	} else {
		progress(100, "existing installation imported")
	}
	result := map[string]any{"path": path, "root_index": rootIndex, "port": record.RCONPort, "managed": !likelyRunning,
		"likely_running": likelyRunning, "requires_controlled_restart": likelyRunning}
	if likelyRunning {
		result["pid"] = external.PID
		result["status"] = "external"
	} else {
		result["status"] = "stopped"
	}
	if steam := e.steamClient(); steam != nil {
		if buildID, source, buildErr := steam.InstalledBuildID(path, L4D2DedicatedAppID); buildErr == nil {
			result["build_id"], result["build_source"] = buildID, source
		}
	}
	return result, nil
}

func (e *Executor) backup(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	launch, err := backupLaunchFromRecord(record, path)
	if err != nil {
		return nil, fmt.Errorf("capture portable launch configuration: %w", err)
	}
	dir := filepath.Join(e.config.DataDir, "backups", safeLogName(job.InstanceID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	kind := strings.ToLower(payload.Kind)
	if kind == "" {
		kind = "daily"
	}
	if kind != "daily" && kind != "weekly" && kind != "checkpoint" && kind != "manual" {
		return nil, errors.New("backup kind must be daily, weekly, checkpoint, or manual")
	}
	name := kind + "-" + time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + safeLogName(job.ID) + ".zip"
	destination, err := ResolveWithin(dir, name)
	if err != nil {
		return nil, err
	}
	progress(20, "creating portable backup")
	manifest, err := CreateBackupWithLaunch(job.InstanceID, path, destination, launch)
	if err != nil {
		return nil, err
	}
	retentionDaily, retentionWeekly := payload.RetentionDaily, payload.RetentionWeekly
	if retentionDaily <= 0 {
		retentionDaily = 7
	}
	if retentionWeekly <= 0 {
		retentionWeekly = 4
	}
	pruned, weeklyPath, err := ApplyBackupRetention(dir, destination, kind, retentionDaily, retentionWeekly)
	if err != nil {
		return nil, err
	}
	st, statErr := os.Stat(destination)
	if statErr != nil {
		return nil, statErr
	}
	progress(100, "backup created")
	archiveHash, _, hashErr := hashFile(destination)
	if hashErr != nil {
		return nil, hashErr
	}
	result := map[string]any{"path": destination, "name": name, "kind": kind, "size": st.Size(), "sha256": archiveHash, "platform": manifest.OS + "/" + manifest.Arch, "format_version": manifest.Version, "files": len(manifest.Files), "created_at": manifest.CreatedAt, "pruned": len(pruned), "pruned_paths": pruned}
	if weeklyPath != "" {
		weeklyHash, weeklySize, weeklyErr := hashFile(weeklyPath)
		if weeklyErr != nil {
			return nil, weeklyErr
		}
		result["weekly"] = map[string]any{"path": weeklyPath, "name": filepath.Base(weeklyPath), "kind": "weekly", "size": weeklySize, "sha256": weeklyHash, "platform": manifest.OS + "/" + manifest.Arch, "created_at": manifest.CreatedAt}
	}
	return result, nil
}

func (e *Executor) restore(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	if e.processes.HasActiveProcess(job.InstanceID) {
		return nil, errors.New("instance must be stopped before restore")
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	archive := ""
	uploadedBackup := payload.UploadID != ""
	if uploadedBackup {
		archive, err = e.resolveUploadedBackup(payload.UploadID, payload.ExpectedSHA256, payload.ExpectedSize)
	} else {
		archive = payload.Archive
		if archive == "" {
			archive = payload.Path
		}
		if archive == "" {
			return nil, errors.New("backup archive path is required")
		}
		if !filepath.IsAbs(archive) {
			archive, err = ResolveWithin(filepath.Join(e.config.DataDir, "backups"), archive)
		} else {
			archive, err = resolveBelow(e.config.DataDir, archive)
		}
	}
	if err != nil {
		return nil, err
	}
	if payload.ExpectedSHA256 != "" {
		if len(payload.ExpectedSHA256) != 64 {
			return nil, errors.New("expected_sha256 must contain 64 hexadecimal characters")
		}
		if _, decodeErr := hex.DecodeString(payload.ExpectedSHA256); decodeErr != nil {
			return nil, errors.New("expected_sha256 must contain 64 hexadecimal characters")
		}
		actual, _, hashErr := hashFile(archive)
		if hashErr != nil {
			return nil, hashErr
		}
		if !strings.EqualFold(actual, payload.ExpectedSHA256) {
			return nil, errors.New("backup archive SHA-256 does not match expected_sha256")
		}
	}
	inspectedManifest, err := ReadBackupManifest(archive)
	if err != nil {
		return nil, err
	}
	rollbackState, err := captureRestoreRollbackState(record, path)
	if err != nil {
		return nil, fmt.Errorf("capture pre-restore instance state: %w", err)
	}
	progress(10, "creating pre-restore checkpoint")
	checkpointJob := backupJobWithKind(job, "checkpoint")
	checkpointJob.ID = job.ID + "-checkpoint"
	checkpoint, err := e.backup(ctx, checkpointJob, func(int, string) {})
	if err != nil {
		return nil, err
	}
	checkpointPath := stringFromMap(checkpoint, "path")
	if checkpointPath == "" {
		return nil, errors.New("pre-restore checkpoint did not return a recoverable path")
	}
	failRestore := func(stage string, cause error) error {
		return e.restoreFailureWithState(job, checkpointPath, stage, cause, rollbackState)
	}
	stagingDir, err := ensureDataSubdirectory(e.config.DataDir, "staging")
	if err != nil {
		return nil, err
	}
	stagingRoot, err := ResolveWithin(stagingDir, "restore-"+safeLogName(job.ID))
	if err != nil {
		return nil, err
	}
	crossPlatform := inspectedManifest.OS != runtime.GOOS
	reinstalledComponents := make([]string, 0)
	if crossPlatform {
		manifestFiles := make([]string, 0, len(inspectedManifest.Files))
		for relative := range inspectedManifest.Files {
			manifestFiles = append(manifestFiles, relative)
		}
		components := coreComponentsForNativeFiles(manifestFiles)
		for index, component := range components {
			progress(25+index*30/max(len(components), 1), "installing "+component+" for target platform before portable overlay")
			componentJob := job
			componentJob.ID = job.ID + "-restore-" + component
			componentJob.Type = "install_mod"
			componentJob.Payload = map[string]any{"kind": component, "rcon_password": payload.RCONPassword, "instance": payload.Instance}
			if _, componentErr := e.installMod(ctx, componentJob, func(int, string) {}); componentErr != nil {
				return nil, failRestore("install "+component+" for cross-platform restore", componentErr)
			}
			reinstalledComponents = append(reinstalledComponents, component)
		}
	}
	progress(65, "validating and applying portable backup overlay")
	manifest, skippedBinaries, err := RestoreBackup(path, archive, stagingRoot)
	if err != nil {
		return nil, failRestore("restore portable backup overlay", err)
	}
	launchRestored := false
	if manifest.Launch != nil {
		progress(90, "restoring source launch configuration with target secrets")
		if _, _, err := e.applyBackupLaunch(record, *manifest.Launch, payload); err != nil {
			return nil, failRestore("restore source launch configuration", err)
		}
		launchRestored = true
	}
	result := map[string]any{"restored_files": len(manifest.Files) - len(skippedBinaries), "format_version": manifest.Version, "launch_config_restored": launchRestored, "legacy_launch_preserved": manifest.Version == backupLegacyFormatVersion, "source_platform": manifest.OS + "/" + manifest.Arch, "cross_platform": crossPlatform, "skipped_binaries": skippedBinaries, "reinstalled_components": reinstalledComponents, "requires_extension_reinstall": crossPlatform && len(skippedBinaries) > 0 && len(reinstalledComponents) == 0, "requires_native_extension_review": crossPlatform && len(skippedBinaries) > 0, "checkpoint": checkpoint}
	if manifest.Launch != nil {
		// Safe for Control persistence: BackupLaunchConfig has no password fields.
		// Control uses this projection so later start jobs do not overwrite the
		// restored source settings with the target's pre-restore configuration.
		result["restored_launch_config"] = manifest.Launch
	}
	if uploadedBackup {
		progress(98, "cleaning completed backup upload")
		result["upload_id"] = payload.UploadID
		if cleanupErr := removeBackupUploadFiles(e.config.DataDir, payload.UploadID); cleanupErr != nil {
			result["upload_cleaned"] = false
			result["cleanup_warning"] = "restored upload will be retried by staging retention cleanup"
		} else {
			result["upload_cleaned"] = true
		}
	}
	progress(100, "backup restored")
	return result, nil
}

func (e *Executor) restoreFailure(job domain.Job, checkpointPath, stage string, cause error) error {
	rollbackErr := e.rollbackServerExtension(job, checkpointPath, false)
	if rollbackErr != nil {
		return fmt.Errorf("%s failed: %w; pre-restore checkpoint rollback also failed: %v", stage, cause, rollbackErr)
	}
	return fmt.Errorf("%s failed; pre-restore checkpoint recovered: %w", stage, cause)
}

func coreComponentsForNativeFiles(files []string) []string {
	hasMetamod, hasSourceMod := false, false
	for _, file := range files {
		lower := "/" + strings.ToLower(filepath.ToSlash(file)) + "/"
		if strings.Contains(lower, "/addons/metamod/") || strings.Contains(lower, "/addons/metamod.") {
			hasMetamod = true
		}
		if strings.Contains(lower, "/addons/sourcemod/") {
			hasSourceMod = true
		}
	}
	result := make([]string, 0, 2)
	if hasMetamod {
		result = append(result, "metamod")
	}
	if hasSourceMod {
		result = append(result, "sourcemod")
	}
	return result
}

func stringFromMap(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func (e *Executor) list(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	files, err := ListManagedFiles(path, firstNonEmpty(payload.Relative, payload.Path))
	if err != nil {
		return nil, err
	}
	progress(100, "files listed")
	return map[string]any{"files": files}, nil
}

func (e *Executor) file(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	path, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	relative := firstNonEmpty(payload.Relative, payload.Path)
	switch strings.ToLower(payload.Operation) {
	case "", "read":
		content, size, digest, err := ReadManagedFile(path, relative)
		if err != nil {
			return nil, err
		}
		progress(100, "file read")
		return map[string]any{"content_base64": content, "size": size, "sha256": digest, "path": relative}, nil
	case "write":
		if err := verifyManagedFilePrecondition(path, relative, payload.ExpectedSHA256); err != nil {
			return nil, err
		}
		if err := WriteManagedFile(path, relative, payload.Content, payload.ContentBase64); err != nil {
			return nil, err
		}
		_, size, digest, err := ReadManagedFile(path, relative)
		if err != nil {
			return nil, err
		}
		progress(100, "file written")
		return map[string]any{"path": relative, "size": size, "sha256": digest}, nil
	case "delete":
		if err := verifyManagedFilePrecondition(path, relative, payload.ExpectedSHA256); err != nil {
			return nil, err
		}
		if err := DeleteManagedFile(path, relative); err != nil {
			return nil, err
		}
		progress(100, "file deleted")
		return map[string]any{"path": relative}, nil
	default:
		return nil, errors.New("file operation must be read, write, or delete")
	}
}

func verifyManagedFilePrecondition(instancePath, relative, expectedSHA256 string) error {
	expectedSHA256 = strings.TrimSpace(expectedSHA256)
	if expectedSHA256 == "" {
		return nil
	}
	if len(expectedSHA256) != sha256.Size*2 {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	_, _, actual, err := ReadManagedFile(instancePath, relative)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expectedSHA256) {
		return errors.New("managed file has changed since it was read")
	}
	return nil
}

func (e *Executor) rcon(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	if payload.RCONPassword == "" {
		return nil, errors.New("RCON password was not supplied")
	}
	host, port := rconEndpoint(record, payload)
	if port <= 0 || port > 65535 {
		return nil, errors.New("invalid RCON port")
	}
	progress(30, "sending RCON command")
	response, err := (RCONClient{}).Execute(ctx, net.JoinHostPort(host, strconv.Itoa(port)), payload.RCONPassword, payload.Command)
	if err != nil {
		return nil, err
	}
	progress(100, "RCON command completed")
	return map[string]any{"response": response}, nil
}

type RCONPlayer struct {
	UserID    int    `json:"user_id"`
	Name      string `json:"name"`
	SteamID   string `json:"steam_id,omitempty"`
	Connected string `json:"connected,omitempty"`
	Ping      int    `json:"ping,omitempty"`
	Bot       bool   `json:"bot,omitempty"`
}

var statusPlayerPattern = regexp.MustCompile(`(?m)^#\s+(?:[0-9]+\s+)?([0-9]+)\s+"((?:\\.|[^"])*)"\s+(\S+)\s+(\S+)\s+([0-9]+)\s+[0-9]+\s+\S+\s+[0-9]+(?:\s+\S+)?\s*$`)

func (e *Executor) rconPlayers(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	if payload.RCONPassword == "" {
		return nil, errors.New("RCON password was not supplied")
	}
	host, port := rconEndpoint(record, payload)
	if port <= 0 || port > 65535 {
		return nil, errors.New("invalid RCON port")
	}
	progress(30, "querying players through RCON")
	response, err := (RCONClient{}).Execute(ctx, net.JoinHostPort(host, strconv.Itoa(port)), payload.RCONPassword, "status")
	if err != nil {
		return nil, err
	}
	players := parseRCONStatusPlayers(response)
	progress(100, "player query completed")
	return map[string]any{"players": players, "count": len(players)}, nil
}

func parseRCONStatusPlayers(status string) []RCONPlayer {
	matches := statusPlayerPattern.FindAllStringSubmatch(status, -1)
	players := make([]RCONPlayer, 0, len(matches))
	for _, match := range matches {
		userID, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		name := strings.ReplaceAll(strings.ReplaceAll(match[2], `\"`, `"`), `\\`, `\`)
		identity := match[3]
		bot := strings.EqualFold(identity, "BOT")
		if !bot && !steamIdentityPattern.MatchString(identity) {
			continue
		}
		ping, _ := strconv.Atoi(match[5])
		player := RCONPlayer{UserID: userID, Name: name, Connected: match[4], Ping: ping, Bot: bot}
		if !bot {
			player.SteamID = identity
		}
		players = append(players, player)
	}
	return players
}

var steamIdentityPattern = regexp.MustCompile(`^(?:STEAM_[0-5]:[01]:[0-9]+|\[U:1:[0-9]+\]|[0-9]{5,20})$`)

func (e *Executor) playerAction(ctx context.Context, job domain.Job, action string, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	quote := func(value string) (string, error) {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n;") {
			return "", errors.New("player value contains forbidden characters")
		}
		return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`, nil
	}
	var command string
	switch action {
	case "kick_player":
		target := payload.SteamID
		if target != "" {
			if !steamIdentityPattern.MatchString(target) {
				return nil, errors.New("invalid Steam identity")
			}
		} else {
			target = payload.Name
		}
		target, err = quote(target)
		if err != nil {
			return nil, err
		}
		reason := payload.Reason
		if reason == "" {
			reason = "Removed by an administrator"
		}
		reason, err = quote(reason)
		if err != nil {
			return nil, err
		}
		if payload.SteamID != "" {
			command = "kickid " + target + " " + reason
		} else {
			command = "kick " + target + " " + reason
		}
	case "ban_player":
		if !steamIdentityPattern.MatchString(payload.SteamID) {
			return nil, errors.New("a valid Steam identity is required")
		}
		if payload.DurationMinutes < 0 || payload.DurationMinutes > 525600 {
			return nil, errors.New("ban duration must be between 0 and 525600 minutes")
		}
		command = fmt.Sprintf("banid %d %s kick; writeid", payload.DurationMinutes, payload.SteamID)
	case "unban_player":
		if !steamIdentityPattern.MatchString(payload.SteamID) {
			return nil, errors.New("a valid Steam identity is required")
		}
		command = "removeid " + payload.SteamID + "; writeid"
	case "change_map":
		if !sourceIdentifier.MatchString(payload.Map) {
			return nil, errors.New("invalid map identifier")
		}
		command = "changelevel " + payload.Map
	default:
		return nil, errors.New("unsupported player action")
	}
	payload.Command = command
	copyJob := job
	raw, _ := json.Marshal(payload)
	_ = json.Unmarshal(raw, &copyJob.Payload)
	result, err := e.rcon(ctx, copyJob, progress)
	if err != nil {
		return nil, err
	}
	result["action"] = action
	return result, nil
}

func (e *Executor) a2sInfo(ctx context.Context, job domain.Job) (map[string]any, error) {
	payload, _ := decodeOperation(job)
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	host, port := rconEndpoint(record, payload)
	info, err := (A2SClient{}).Info(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	e.telemetryMu.Lock()
	if e.telemetry == nil {
		e.telemetry = make(map[string]instanceTelemetry)
	}
	e.telemetry[job.InstanceID] = instanceTelemetry{players: info.Players, observedAt: time.Now().UTC()}
	e.telemetryMu.Unlock()
	return map[string]any{"info": info}, nil
}

func (e *Executor) cachedPlayers(instanceID string, now time.Time) int {
	if e == nil {
		return 0
	}
	e.telemetryMu.RLock()
	entry, ok := e.telemetry[instanceID]
	e.telemetryMu.RUnlock()
	if !ok || now.Sub(entry.observedAt) > 2*time.Minute || entry.observedAt.After(now.Add(10*time.Second)) {
		return 0
	}
	return entry.players
}

func (e *Executor) a2sPlayers(ctx context.Context, job domain.Job) (map[string]any, error) {
	payload, _ := decodeOperation(job)
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	host, port := rconEndpoint(record, payload)
	players, err := (A2SClient{}).Players(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return map[string]any{"players": players}, nil
}

func (e *Executor) deleteInstance(_ context.Context, job domain.Job, detach bool, progress ProgressFunc) (map[string]any, error) {
	payload, _ := decodeOperation(job)
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return map[string]any{"detached": true}, nil
	}
	if processStatusActive(e.processes.State(job.InstanceID).Status) {
		return nil, errors.New("stop the instance before deleting or detaching it")
	}
	mode := payload.Mode
	if detach {
		mode = "detach"
	}
	if mode == "" {
		mode = "quarantine"
	}
	result := map[string]any{}
	if mode == "quarantine" {
		source, err := e.instancePath(record)
		if err != nil {
			return nil, err
		}
		quarantinedAt := time.Now().UTC().Truncate(time.Second)
		entryName := safeLogName(job.InstanceID) + "-" + quarantinedAt.Format("20060102T150405Z")
		target, err := PrepareQuarantineTarget(e.paths, record.RootIndex, entryName)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(source, target); err != nil {
			return nil, fmt.Errorf("quarantine instance: %w", err)
		}
		result["quarantine_entry"] = entryName
		result["root_index"] = record.RootIndex
		result["quarantined_at"] = quarantinedAt.Format(time.RFC3339)
		result["retention_expires_at"] = quarantinedAt.Add(defaultQuarantineRetention).Format(time.RFC3339)
		result["retention_days"] = 7
	} else if mode != "detach" {
		return nil, errors.New("delete mode must be detach or quarantine")
	}
	if err := e.registry.Delete(job.InstanceID); err != nil {
		return nil, err
	}
	result["detached"] = true
	progress(100, "instance removed from management")
	return result, nil
}

func (e *Executor) purgeQuarantine(job domain.Job, progress ProgressFunc) (map[string]any, error) {
	if job.InstanceID != "" {
		return nil, errors.New("quarantine purge must be node-scoped")
	}
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if payload.QuarantineID == "" {
		return nil, errors.New("quarantine_entry_id is required")
	}
	progress(20, "validating quarantine entry")
	removed, err := PurgeQuarantine(e.paths, payload.RootIndex, payload.QuarantineEntry)
	if err != nil {
		return nil, err
	}
	progress(100, "quarantine entry permanently removed")
	return map[string]any{
		"quarantine_entry": payload.QuarantineEntry,
		"root_index":       payload.RootIndex,
		"removed":          removed,
		"already_absent":   !removed,
	}, nil
}

func (e *Executor) installMod(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(payload.Kind))
	source := firstNonEmpty(payload.SourcePath, payload.Source, payload.SourceID)
	uploadedContent := strings.TrimSpace(payload.ContentBase64)
	if uploadedContent != "" && source != "" {
		return nil, errors.New("provide either an uploaded package or a package source, not both")
	}
	if kind == "workshop" || kind == "workshop_item" {
		if uploadedContent != "" {
			return nil, errors.New("Workshop packages cannot be uploaded directly")
		}
		if source == "" {
			return nil, errors.New("Workshop item or collection id is required")
		}
		return e.installWorkshop(ctx, job, source, progress)
	}

	official := false
	officialVersion := ""
	if source == "" && uploadedContent == "" {
		if kind != "sourcemod" && kind != "metamod" {
			return nil, errors.New("package source is required")
		}
		resolver := e.officialPackageResolver
		if resolver == nil {
			resolver = ResolveOfficialPackage
		}
		progress(5, "resolving official stable extension package")
		resolved, err := resolver(ctx, kind)
		if err != nil {
			return nil, err
		}
		if resolved.Kind != kind || (resolved.OS != "" && resolved.OS != runtime.GOOS) {
			return nil, errors.New("official package resolver returned mismatched package metadata")
		}
		if err := validateOfficialAssetURL(resolved.URL); err != nil {
			return nil, err
		}
		source, official, officialVersion = resolved.URL, true, resolved.Version
	}
	allowNative := payload.RiskConfirmed || payload.AcknowledgeRisk
	componentKind := strings.TrimSuffix(kind, "_url")
	if (componentKind == "sourcemod" || componentKind == "metamod") && !official && !allowNative {
		return nil, errors.New("third-party server extension packages require acknowledge_native_risk")
	}
	if (componentKind == "sourcemod" || componentKind == "metamod") && payload.RCONPassword == "" {
		return nil, errors.New("RCON password is required to verify a server extension installation")
	}
	localSource := source
	downloadedHash := ""
	downloadedSize := int64(0)
	if uploadedContent != "" {
		if payload.Name == "" || filepath.Base(payload.Name) != payload.Name || strings.ContainsAny(payload.Name, "\x00/\\") {
			return nil, errors.New("uploaded package name is invalid")
		}
		if base64.StdEncoding.DecodedLen(len(uploadedContent)) > maxManagedFileSize {
			return nil, errors.New("uploaded package exceeds 8 MiB")
		}
		raw, decodeErr := base64.StdEncoding.DecodeString(uploadedContent)
		if decodeErr != nil || len(raw) == 0 || len(raw) > maxManagedFileSize {
			return nil, errors.New("uploaded package is not valid Base64 or exceeds 8 MiB")
		}
		stagingDir, stagingErr := ensureDataSubdirectory(e.config.DataDir, "staging")
		if stagingErr != nil {
			return nil, stagingErr
		}
		localSource, err = ResolveWithin(stagingDir, "upload-"+safeLogName(job.ID)+strings.ToLower(filepath.Ext(payload.Name)))
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(localSource), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(localSource, raw, 0o600); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		downloadedHash, downloadedSize = hex.EncodeToString(digest[:]), int64(len(raw))
		source = payload.Name
		defer os.Remove(localSource)
	} else if strings.HasPrefix(strings.ToLower(source), "https://") {
		extension := filepath.Ext(strings.Split(source, "?")[0])
		if extension == "" || len(extension) > 8 {
			extension = ".package"
		}
		stagingDir, stagingErr := ensureDataSubdirectory(e.config.DataDir, "staging")
		if stagingErr != nil {
			return nil, stagingErr
		}
		localSource, err = ResolveWithin(stagingDir, "download-"+safeLogName(job.ID)+extension)
		if err != nil {
			return nil, err
		}
		progress(10, "downloading package")
		downloader := e.packageDownloader
		if downloader == nil {
			downloader = func(ctx context.Context, source, destination string, official bool) (string, int64, error) {
				if official {
					return DownloadOfficialHTTPS(ctx, source, destination)
				}
				return DownloadHTTPS(ctx, source, destination)
			}
		}
		downloadedHash, downloadedSize, err = downloader(ctx, source, localSource, official)
		if err != nil {
			return nil, err
		}
		defer os.Remove(localSource)
	} else if !filepath.IsAbs(localSource) {
		return nil, errors.New("uploaded package must be an absolute path inside data_dir")
	} else {
		localSource, err = resolveBelow(e.config.DataDir, localSource)
		if err != nil {
			return nil, errors.New("uploaded package escapes data_dir through its path or a link")
		}
	}
	if kind == "" {
		if strings.EqualFold(filepath.Ext(localSource), ".vpk") {
			kind = "vpk"
		} else {
			kind = "sourcemod"
		}
	}
	if strings.Contains(kind, "vpk") {
		version, hash, err := validateVPK(localSource)
		if err != nil {
			return nil, err
		}
		if downloadedHash != "" && !strings.EqualFold(downloadedHash, hash) {
			return nil, errors.New("download hash changed during validation")
		}
		name := filepath.Base(strings.Split(source, "?")[0])
		if !strings.HasSuffix(strings.ToLower(name), ".vpk") {
			name = safeLogName(job.ID) + ".vpk"
		}
		targetRelative := filepath.ToSlash(filepath.Join("left4dead2", "addons", name))
		progress(70, "installing VPK")
		previous, pruned, err := e.installVersionedPackage(ctx, job, payload, "vpk", instancePath, localSource, targetRelative, hash, progress)
		if err != nil {
			return nil, err
		}
		progress(100, "VPK installed")
		result := map[string]any{"kind": "vpk", "path": targetRelative, "sha256": hash, "size": downloadedSize, "vpk_version": version}
		if previous != nil {
			result["previous_version"] = previous.result()
		}
		if len(pruned) > 0 {
			result["pruned_snapshots"] = pruned
		}
		return result, nil
	}
	if kind == "smx" || kind == "plugin" || kind == "native" {
		extension := strings.ToLower(filepath.Ext(localSource))
		packageName := filepath.Base(localSource)
		if uploadedContent != "" {
			extension = strings.ToLower(filepath.Ext(payload.Name))
			packageName = payload.Name
		}
		destinationDir := filepath.Join("left4dead2", "addons", "sourcemod", "plugins")
		if extension == ".dll" || extension == ".so" {
			if !allowNative {
				return nil, errors.New("native extension requires acknowledge_native_risk")
			}
			destinationDir = filepath.Join("left4dead2", "addons", "sourcemod", "extensions")
		} else if extension != ".smx" {
			return nil, errors.New("plugin package must be .smx, .dll, or .so")
		}
		targetRelative := filepath.ToSlash(filepath.Join(destinationDir, packageName))
		hash, packageSize, err := hashFile(localSource)
		if err != nil {
			return nil, err
		}
		if extension == ".smx" {
			previous, pruned, err := e.installVersionedPackage(ctx, job, payload, "plugin", instancePath, localSource, targetRelative, hash, progress)
			if err != nil {
				return nil, err
			}
			progress(100, "plugin installed")
			result := map[string]any{"kind": "plugin", "path": targetRelative, "sha256": hash, "size": packageSize}
			if previous != nil {
				result["previous_version"] = previous.result()
			}
			if len(pruned) > 0 {
				result["pruned_snapshots"] = pruned
			}
			return result, nil
		}
		if _, err := ResolveWithin(instancePath, filepath.FromSlash(targetRelative)); err != nil {
			return nil, err
		}
		if err := copyRegularFileToRootNoFollow(localSource, instancePath, targetRelative, secureWriteOptions{
			Mode: 0o640, ExpectedSHA256: hash, Replace: true, RejectExistingLink: true,
		}); err != nil {
			return nil, err
		}
		progress(100, "plugin installed")
		return map[string]any{"kind": kind, "path": targetRelative, "sha256": hash, "size": packageSize}, nil
	}
	if kind != "sourcemod" && kind != "sourcemod_url" && kind != "metamod" && kind != "metamod_url" {
		return nil, fmt.Errorf("unsupported package kind %q", payload.Kind)
	}
	result, err := e.installServerExtension(ctx, job, payload, record, instancePath, componentKind, localSource, downloadedHash, downloadedSize, officialVersion, progress)
	if err != nil {
		return nil, err
	}
	result["source"] = displayHTTPSURL(source)
	result["official"] = official
	return result, nil
}

func (e *Executor) togglePackage(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	packagePath := strings.TrimSpace(payload.PackagePath)
	if packagePath == "" {
		packagePath = strings.TrimSpace(payload.Path)
	}
	if packagePath == "" {
		return nil, errors.New("package_path is required")
	}
	var absolute string
	if filepath.IsAbs(packagePath) {
		absolute, err = resolveBelow(instancePath, packagePath)
	} else {
		absolute, err = ResolveWithin(instancePath, filepath.FromSlash(packagePath))
	}
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(instancePath, absolute)
	if err != nil {
		return nil, err
	}
	relative = filepath.ToSlash(relative)
	kind := strings.ToLower(payload.Kind)
	if kind == "" {
		if strings.EqualFold(filepath.Ext(relative), ".vpk") {
			kind = "vpk"
		} else if strings.EqualFold(filepath.Ext(relative), ".smx") {
			kind = "sourcemod"
		}
	}
	base := filepath.Base(relative)
	var enabledPath, disabledPath string
	switch kind {
	case "vpk", "workshop":
		if !strings.HasSuffix(strings.ToLower(base), ".vpk") {
			return nil, errors.New("VPK package path must end in .vpk")
		}
		enabledPath = filepath.ToSlash(filepath.Join("left4dead2", "addons", base))
		disabledPath = filepath.ToSlash(filepath.Join("left4dead2", "addons", "panel-disabled", base))
	case "sourcemod", "smx", "plugin":
		if !strings.HasSuffix(strings.ToLower(base), ".smx") {
			return nil, errors.New("SourceMod package path must end in .smx")
		}
		enabledPath = filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", base))
		disabledPath = filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", "disabled", base))
	default:
		return nil, errors.New("toggle supports VPK and SourceMod SMX packages")
	}
	if relative != enabledPath && relative != disabledPath {
		return nil, errors.New("package_path is not in an approved enabled or disabled package directory")
	}
	targetRelative := disabledPath
	if payload.Enabled {
		targetRelative = enabledPath
	}
	if relative != targetRelative {
		if payload.Enabled && isVPKPackageKind(kind) {
			if err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{{
				Source: absolute, TargetRelative: enabledPath,
			}}); err != nil {
				return nil, fmt.Errorf("cannot enable conflicting VPK: %w", err)
			}
		}
		if err := moveFileWithinNoFollow(instancePath, relative, targetRelative); err != nil {
			return nil, fmt.Errorf("toggle package atomically: %w", err)
		}
	}
	progress(100, "package state updated")
	return map[string]any{"path": targetRelative, "enabled": payload.Enabled, "restart_required": e.processes.State(job.InstanceID).Status == "running"}, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func (e *Executor) instancePath(record InstanceRecord) (string, error) {
	return resolveInstanceRecordPath(e.paths, record)
}

func defaultServerExecutable() string {
	if runtime.GOOS == "windows" {
		return "srcds.exe"
	}
	return "srcds_run"
}

func validateServerExecutable(value string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(value))))
	if cleaned == "." || cleaned != filepath.Base(filepath.FromSlash(cleaned)) {
		return errors.New("server executable must be a supported filename in the instance root")
	}
	allowed := map[string]bool{"srcds_run": true, "srcds_linux": true}
	if runtime.GOOS == "windows" {
		allowed = map[string]bool{"srcds.exe": true}
	}
	if !allowed[strings.ToLower(cleaned)] {
		return errors.New("server executable is not supported on this operating system")
	}
	return nil
}

func buildServerConfig(config domain.InstanceConfig, rconPassword string) (string, error) {
	var b strings.Builder
	line := func(name, value string) error {
		if !sourceIdentifier.MatchString(name) {
			return fmt.Errorf("invalid CVar name %q", name)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid value for %s", name)
		}
		value = strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`)
		fmt.Fprintf(&b, "%s \"%s\"\n", name, value)
		return nil
	}
	if config.Hostname != "" {
		if err := line("hostname", config.Hostname); err != nil {
			return "", err
		}
	}
	if err := line("rcon_password", rconPassword); err != nil {
		return "", err
	}
	if config.ServerPassword != "" {
		if err := line("sv_password", config.ServerPassword); err != nil {
			return "", err
		}
	}
	if config.Difficulty != "" {
		if err := line("z_difficulty", config.Difficulty); err != nil {
			return "", err
		}
	}
	if config.Region != "" {
		if err := line("sv_region", config.Region); err != nil {
			return "", err
		}
	}
	if config.SteamGroupID != "" {
		if err := line("sv_steamgroup", config.SteamGroupID); err != nil {
			return "", err
		}
	}
	keys := make([]string, 0, len(config.CVars))
	for key := range config.CVars {
		if isManagedSecretCVar(key) {
			return "", fmt.Errorf("CVar %q is managed by the encrypted secret store", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := line(key, config.CVars[key]); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func isManagedSecretCVar(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "rcon_password", "sv_password":
		return true
	default:
		return false
	}
}

func rconEndpoint(record InstanceRecord, payload operationPayload) (string, int) {
	host := localGameEndpointHost(payload.RCONHost, record.RCONHost)
	port := payload.RCONPort
	if port == 0 {
		port = payload.Port
	}
	if port == 0 {
		port = record.RCONPort
	}
	return host, port
}

// Game query and RCON operations always target the Agent host itself. Node
// payloads can contain a wildcard bind address, but they must never turn these
// fixed protocol clients into an arbitrary network/metadata endpoint.
func localGameEndpointHost(candidates ...string) string {
	for _, candidate := range candidates {
		candidate = strings.Trim(strings.TrimSpace(candidate), "[]")
		ip := net.ParseIP(candidate)
		if ip == nil || !ip.IsLoopback() {
			continue
		}
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return "127.0.0.1"
}

func payloadString(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return strings.ToLower(value)
	}
	return ""
}

func exactPayloadString(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func backupJobWithKind(job domain.Job, kind string) domain.Job {
	copyJob := job
	copyJob.Payload = make(map[string]any, len(job.Payload)+1)
	for key, value := range job.Payload {
		copyJob.Payload[key] = value
	}
	copyJob.Payload["kind"] = kind
	return copyJob
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func processStatusActive(status string) bool {
	return status == "running" || status == "starting" || status == "stopping" || status == "degraded"
}

type keyedLocker struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func newKeyedLocker() *keyedLocker { return &keyedLocker{locks: make(map[string]chan struct{})} }
func (l *keyedLocker) acquire(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	lock := l.locks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		l.locks[key] = lock
	}
	l.mu.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
