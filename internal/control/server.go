package control

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/l4d2-panel/l4d2panel/internal/containerupdate"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/portableupdate"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
	"github.com/l4d2-panel/l4d2panel/internal/version"
)

//go:embed static/*
var embeddedStatic embed.FS

const (
	sessionCookieName   = "l4d2panel_session"
	bootstrapCookieName = "l4d2panel_bootstrap"
)

type contextKey int

const principalContextKey contextKey = 1

type Server struct {
	config                   Config
	store                    *Store
	events                   *EventBroker
	agents                   *AgentHub
	logger                   *slog.Logger
	limiter                  *loginLimiter
	handler                  http.Handler
	revalidateEventPrincipal func(context.Context, Principal) (Principal, error)
	eventsAfter              func(context.Context, int64, int) ([]Event, error)
	eventAuthRefreshInterval time.Duration
	eventCatchupInterval     time.Duration
	updates                  systemUpdateManager
}

func NewServer(config Config, store *Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	events := NewEventBroker(store)
	s := &Server{config: config, store: store, events: events, logger: logger, limiter: newLoginLimiter()}
	if containerupdate.Configured() {
		s.updates = containerupdate.Detect(config.DatabasePath, version.Version)
	} else {
		s.updates = portableupdate.Detect(config.ConfigPath, config.DatabasePath, version.Version, config.PortableService)
	}
	s.revalidateEventPrincipal = store.RevalidatePrincipal
	s.eventsAfter = store.EventsAfter
	s.eventAuthRefreshInterval = time.Second
	s.eventCatchupInterval = 2 * time.Second
	s.agents = NewAgentHub(store, events, logger, config.MasterKey)
	s.handler = s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Close() { s.agents.Close() }

func (s *Server) RunHousekeeping(ctx context.Context) {
	s.reconcileSystemUpdates(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileSystemUpdates(ctx)
			if err := s.store.AggregateMetrics(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("aggregate monitoring metrics", "error", err)
			}
			if err := s.store.Housekeeping(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("control-plane housekeeping failed", "error", err)
			}
			if stale, err := s.store.MarkStaleNodes(ctx); err != nil {
				s.logger.Error("mark stale nodes", "error", err)
			} else {
				for _, node := range stale {
					_ = s.store.CreateNotification(ctx, "critical", "node.offline", "Node offline", node.Name+" stopped reporting heartbeats", node.ID, nil)
					_, _ = s.events.Publish(ctx, Event{Type: "node.offline", ResourceType: "node", ResourceID: node.ID,
						Data: map[string]any{"node_id": node.ID}}, true)
				}
			}
			s.runMonitoringProbes(ctx)
			s.runDueSchedules(ctx)
			s.processNotificationDeliveries(ctx)
		}
	}
}

func (s *Server) runDueSchedules(ctx context.Context) {
	schedules, err := s.store.DueSchedules(ctx, 50)
	if err != nil {
		s.logger.Error("load due schedules", "error", err)
		return
	}
	for _, schedule := range schedules {
		ranAt := time.Now().UTC()
		if !scheduledActionAllowed(schedule.Action) {
			s.logger.Error("refusing unsafe persisted schedule action", "schedule_id", schedule.ID, "action", schedule.Action)
			if err := s.store.AdvanceSchedule(ctx, schedule, ranAt); err != nil {
				s.logger.Error("advance rejected schedule", "schedule_id", schedule.ID, "error", err)
			}
			continue
		}
		key := "schedule:" + schedule.ID + ":" + schedule.NextRunAt.UTC().Format(time.RFC3339)
		payload := cloneMap(schedule.Payload)
		action := schedule.Action
		if action == "broadcast" {
			command, commandErr := safeBroadcastCommand(stringFromAny(payload["message"]))
			if commandErr != nil {
				s.logger.Error("refusing invalid scheduled broadcast", "schedule_id", schedule.ID, "error", commandErr)
				_ = s.store.AdvanceSchedule(ctx, schedule, ranAt)
				continue
			}
			action, payload = "rcon", map[string]any{"command": command}
		}
		if isLifecycleAction(action) {
			instance, instanceErr := s.store.GetInstance(ctx, schedule.InstanceID)
			if instanceErr != nil {
				s.logger.Error("load scheduled instance", "schedule_id", schedule.ID, "error", instanceErr)
				_ = s.store.AdvanceSchedule(ctx, schedule, ranAt)
				continue
			}
			payload, err = canonicalLifecyclePayload(action, payload, instance)
			if err != nil {
				s.logger.Error("refusing invalid persisted schedule payload", "schedule_id", schedule.ID, "error", err)
				_ = s.store.AdvanceSchedule(ctx, schedule, ranAt)
				continue
			}
		}
		if action == "backup" {
			managedPaths, managedErr := s.store.BackupManagedPackagePaths(ctx, schedule.InstanceID)
			if managedErr != nil {
				s.logger.Error("load managed package paths for scheduled backup", "schedule_id", schedule.ID, "error", managedErr)
				_ = s.store.AdvanceSchedule(ctx, schedule, ranAt)
				continue
			}
			payload["backup_scope"] = protocol.BackupScopeConfigAndManagedPackages
			payload["registered_paths"] = managedPaths
		}
		job, _, queueErr := s.store.QueueInstanceJob(ctx, schedule.InstanceID, action, payload, "", key)
		if queueErr == nil {
			if action == "start" || action == "stop" || action == "restart" {
				if projectionErr := s.publishInstanceProjection(ctx, schedule.InstanceID, job.ID); projectionErr != nil {
					s.logger.Error("publish scheduled lifecycle projection", "schedule_id", schedule.ID, "job_id", job.ID, "error", projectionErr)
				}
			}
			_ = s.store.WriteAudit(ctx, domain.AuditEvent{ActorName: "scheduler", Action: "schedule.triggered", ResourceType: "schedule",
				ResourceID: schedule.ID, Metadata: map[string]any{"job_id": job.ID, "instance_id": schedule.InstanceID}})
			s.dispatchSoon(job.NodeID)
		} else if !errors.Is(queueErr, ErrConflict) {
			s.logger.Error("queue scheduled job", "schedule_id", schedule.ID, "error", queueErr)
		}
		if err := s.store.AdvanceSchedule(ctx, schedule, ranAt); err != nil {
			s.logger.Error("advance schedule", "schedule_id", schedule.ID, "error", err)
		}
	}
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.realIP)
	r.Use(middleware.Recoverer)
	r.Use(s.securityHeaders)
	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)
	r.Get("/api/openapi.yaml", s.handleOpenAPIYAML)
	r.Get("/docs/openapi.yaml", s.handleOpenAPIYAML)
	r.Get("/agent/v1/connect", s.handleAgentConnect)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/openapi.json", s.handleOpenAPI)
		r.Get("/public/servers", s.handlePublicServers)
		r.Post("/agent/enroll", s.handleAgentEnroll)
		r.Get("/auth/bootstrap", s.handleBootstrapStatus)
		r.Post("/auth/bootstrap", s.handleBootstrapOwner)
		r.Post("/auth/login", s.handleLogin)
		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)
			r.Use(s.verifyCSRF)
			r.Use(s.enforceActivatedAccount)
			r.Use(s.enforceTokenResourceScope)
			r.Get("/auth/me", s.handleMe)
			r.With(s.requireInteractiveSession).Post("/auth/logout", s.handleLogout)
			r.With(s.requireInteractiveSession).Post("/auth/activate", s.handleCompleteActivation)
			r.With(s.requireInteractiveSession).Post("/auth/totp/setup", s.handleTOTPSetup)
			r.With(s.requireInteractiveSession).Post("/auth/totp/confirm", s.handleTOTPConfirm)
			r.With(s.requireInteractiveSession).Get("/api-tokens", s.handleListAPITokens)
			r.With(s.requireInteractiveSession).Post("/api-tokens", s.handleCreateAPIToken)
			r.With(s.requireInteractiveSession).Delete("/api-tokens/{tokenID}", s.handleDeleteAPIToken)
			r.Get("/events", s.handleEvents)

			r.With(s.requirePermission(PermissionNodesRead)).Get("/nodes", s.handleListNodes)
			r.With(s.requireNodePermission(PermissionNodesRead)).Get("/nodes/{nodeID}", s.handleGetNode)
			r.With(s.requireGlobalPermission(PermissionNodesEnroll)).Post("/nodes/enrollment-tokens", s.handleCreateEnrollmentToken)
			r.With(s.requireNodePermission(PermissionNodesUpdate)).Patch("/nodes/{nodeID}", s.handlePatchNode)
			r.With(s.requireNodePermission(PermissionNodesCredentialsRotate)).Post("/nodes/{nodeID}/credential/rotate", s.handleRotateNodeCredential)
			r.With(s.requireNodePermission(PermissionNodesDiscover)).Post("/nodes/{nodeID}/discoveries", s.handleCreateNodeDiscovery)
			r.With(s.requireNodePermission(PermissionNodesDiscover)).Get("/nodes/{nodeID}/discoveries/{jobID}", s.handleGetNodeDiscovery)
			r.With(s.requireNodePermission(PermissionNodesQuarantineRead)).Get("/nodes/{nodeID}/quarantine", s.handleListNodeQuarantine)
			r.With(s.requireInteractiveSession, s.requireNodePermission(PermissionNodesQuarantinePurge)).Post("/nodes/{nodeID}/quarantine/{entryID}/purge-confirmations", s.handleCreateQuarantinePurgeConfirmation)
			r.With(s.requireInteractiveSession, s.requireNodePermission(PermissionNodesQuarantinePurge)).Post("/nodes/{nodeID}/quarantine/{entryID}/purge", s.handlePurgeQuarantineEntry)
			r.With(s.requireNodePermission(PermissionNodesDelete)).Delete("/nodes/{nodeID}", s.handleDeleteNode)

			r.With(s.requirePermission(PermissionEnvironmentsRead)).Get("/environments", s.handleListEnvironments)
			r.Get("/environments/{environmentID}", s.handleGetEnvironment)
			r.Post("/environments/{environmentID}/inspect", s.handleInspectEnvironment)
			r.Post("/environments/{environmentID}/steamcmd/actions", s.handleSteamCMDAction)
			r.Get("/environments/{environmentID}/game-installations", s.handleListGameInstallations)
			r.Post("/environments/{environmentID}/game-installations", s.handleCreateGameInstallation)
			r.Post("/environments/{environmentID}/game-installations/{installationID}/actions", s.handleGameInstallationAction)
			r.With(s.requireInteractiveSession).Delete("/environments/{environmentID}/game-installations/{installationID}", s.handleDeleteGameInstallation)

			r.With(s.requirePermission(PermissionInstancesRead)).Get("/instances", s.handleListInstances)
			r.With(s.requireInstancePermission(PermissionInstancesRead)).Get("/instances/{instanceID}", s.handleGetInstance)
			r.With(s.requirePermission(PermissionInstancesCreate)).Post("/instances", s.handleCreateInstance)
			r.With(s.requireInstancePermission(PermissionInstancesConfigure)).Patch("/instances/{instanceID}", s.handlePatchInstance)
			r.With(s.requireInstancePermission(PermissionInstancesDelete)).Delete("/instances/{instanceID}", s.handleDeleteInstance)
			r.With(s.requireInstancePermission(PermissionInstancesRead)).Post("/instances/{instanceID}/actions", s.handleInstanceAction)
			r.With(s.requireInstancePermission(PermissionPlayersRead)).Get("/instances/{instanceID}/players", s.handleInstancePlayers)
			r.With(s.requireInstancePermission(PermissionPlayersRead)).Get("/instances/{instanceID}/bans", s.handleInstanceBans)
			r.With(s.requireInstancePermission(PermissionPackagesRead)).Get("/instances/{instanceID}/packages", s.handleInstancePackages)
			r.With(s.requireInstancePermission(PermissionPackagesRead)).Get("/instances/{instanceID}/packages/{packageID}", s.handlePackageDetails)
			r.With(s.requireInstancePermission(PermissionPackagesInstall)).Post("/instances/{instanceID}/packages/scan", s.handleScanInstalledPackages)
			r.With(s.requireInstancePermission(PermissionPackagesInstall)).Post("/instances/{instanceID}/plugin-archives/inspections", s.handleCreatePluginArchiveInspection)
			r.With(s.requireInstancePermission(PermissionPackagesInstall)).Get("/instances/{instanceID}/plugin-archives/inspections/{jobID}", s.handleGetPluginArchiveInspection)
			r.With(s.requireInstancePermission(PermissionPackagesDelete)).Delete("/instances/{instanceID}/packages/{packageID}", s.handleDeletePackage)
			r.With(s.requireInstancePermission(PermissionPackagesRead)).Get("/instances/{instanceID}/packages/{packageID}/versions", s.handlePackageVersions)
			r.With(s.requireInstancePermission(PermissionPackagesRollback)).Post("/instances/{instanceID}/packages/{packageID}/versions/{versionID}/rollback", s.handleRollbackPackageVersion)
			r.With(s.requireInstancePermission(PermissionBackupsRead)).Get("/instances/{instanceID}/backups", s.handleInstanceBackups)
			r.With(s.requireInstancePermission(PermissionBackupsCreate)).Post("/instances/{instanceID}/plugin-backups", s.handleCreatePluginBackup)
			r.With(s.requireInstancePermission(PermissionBackupsDelete)).Delete("/instances/{instanceID}/backups/{backupID}", s.handleDeleteBackup)
			r.With(s.requireInstancePermission(PermissionBackupsDownload)).Post("/instances/{instanceID}/backups/{backupID}/downloads", s.handleCreateBackupDownload)
			r.With(s.requireInstancePermission(PermissionBackupsDownload)).Get("/instances/{instanceID}/backups/{backupID}/downloads/{jobID}", s.handleGetBackupDownload)
			r.With(s.requireInstancePermission(PermissionBackupsRestore)).Post("/instances/{instanceID}/backup-uploads", s.handleCreateBackupUpload)
			r.With(s.requireInstancePermission(PermissionBackupsRestore)).Get("/instances/{instanceID}/backup-uploads/{uploadID}", s.handleGetBackupUpload)
			r.With(s.requireInstancePermission(PermissionBackupsRestore)).Delete("/instances/{instanceID}/backup-uploads/{uploadID}", s.handleAbortBackupUpload)
			r.With(s.requireInstancePermission(PermissionBackupsRestore)).Post("/instances/{instanceID}/backup-uploads/{uploadID}/chunks", s.handleCreateBackupUploadChunk)
			r.With(s.requireInstancePermission(PermissionBackupsRestore)).Get("/instances/{instanceID}/backup-uploads/{uploadID}/chunks/{jobID}", s.handleGetBackupUploadChunk)
			r.With(s.requireInstancePermission(PermissionBackupsRestore)).Post("/instances/{instanceID}/backup-uploads/{uploadID}/complete", s.handleCompleteBackupUpload)
			r.With(s.requireInstancePermission(PermissionLogsRead)).Post("/instances/{instanceID}/logs/search", s.handleCreateLogSearch)
			r.With(s.requireInstancePermission(PermissionLogsRead)).Get("/instances/{instanceID}/logs/search/{jobID}", s.handleGetLogSearch)
			r.With(s.requireInstancePermission(PermissionLogsRead)).Post("/instances/{instanceID}/logs/downloads", s.handleCreateLogDownload)
			r.With(s.requireInstancePermission(PermissionLogsRead)).Get("/instances/{instanceID}/logs/downloads/{jobID}", s.handleGetLogDownload)
			r.With(s.requireInstancePermission(PermissionSchedulesRead)).Get("/instances/{instanceID}/schedules", s.handleInstanceSchedules)
			r.With(s.requireInstancePermission(PermissionSchedulesManage)).Patch("/instances/{instanceID}/schedules/{scheduleID}", s.handleSetScheduleEnabled)
			r.With(s.requireInstancePermission(PermissionSchedulesManage)).Delete("/instances/{instanceID}/schedules/{scheduleID}", s.handleDeleteSchedule)
			r.With(s.requireInstancePermission(PermissionFilesList)).Get("/instances/{instanceID}/files", s.handleInstanceFiles)
			r.With(s.requireInstancePermission(PermissionFilesRead)).Get("/instances/{instanceID}/files/content", s.handleInstanceFileContent)
			r.With(s.requireInstancePermission(PermissionFilesWrite)).Put("/instances/{instanceID}/files/content", s.handleWriteInstanceFile)
			r.With(s.requireInstancePermission(PermissionFilesWrite)).Put("/instances/{instanceID}/packages/{packageID}/files/content", s.handleWritePackageFile)
			r.With(s.requireInstancePermission(PermissionFilesRead)).Get("/instances/{instanceID}/packages/{packageID}/files/content", s.handlePackageFileContent)
			r.With(s.requireInstancePermission(PermissionFilesDelete)).Delete("/instances/{instanceID}/files/content", s.handleDeleteInstanceFile)
			r.With(s.requireInstancePermission(PermissionPackagesInstall)).Post("/instances/{instanceID}/packages/qq-flash", s.handleImportQQFlashFile)
			r.With(s.requireInstancePermission(PermissionPackagesInstall)).Post("/instances/{instanceID}/files/qq-flash", s.handleImportQQFlashFile)
			r.With(s.requirePermission(PermissionMapsRead)).Get("/maps", s.handleListMaps)
			r.With(s.requirePermission(PermissionMapsRead)).Get("/maps/{mapID}", s.handleGetMap)
			r.With(s.requirePermission(PermissionMapsRead)).Get("/maps/{mapID}/poster", s.handleGetMapPoster)
			r.With(s.requirePermission(PermissionMapsImport)).Post("/nodes/{nodeID}/maps/imports", s.handleImportMap)
			r.With(s.requirePermission(PermissionMapsAssign)).Put("/maps/{mapID}/assignments/{instanceID}", s.handleAssignMap)
			r.With(s.requirePermission(PermissionMapsAssign)).Delete("/maps/{mapID}/assignments/{instanceID}", s.handleUnassignMap)
			r.With(s.requirePermission(PermissionMapsVerify)).Post("/maps/{mapID}/verify", s.handleVerifyMap)
			r.With(s.requireInteractiveSession, s.requirePermission(PermissionMapsDelete)).Post("/maps/{mapID}/delete-confirmations", s.handleCreateMapDeleteConfirmation)
			r.With(s.requireInteractiveSession, s.requirePermission(PermissionMapsDelete)).Delete("/maps/{mapID}", s.handleDeleteMap)
			r.With(s.requirePermission(PermissionJobsRead)).Get("/jobs", s.handleListJobs)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Get("/jobs/terminal-conflicts", s.handleListTerminalOutcomeConflicts)
			r.Get("/jobs/{jobID}", s.handleGetJob)
			r.With(s.requireJobPermission(PermissionJobsCancel)).Post("/jobs/{jobID}/cancel", s.handleCancelJob)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Get("/jobs/{jobID}/terminal-conflict", s.handleGetTerminalOutcomeConflict)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Post("/jobs/{jobID}/terminal-conflict/resolve", s.handleResolveTerminalOutcomeConflict)
			r.Get("/audit", s.handleListAudit)
			r.With(s.requirePermission(PermissionNotificationsRead)).Get("/notifications", s.handleListNotifications)
			r.With(s.requirePermission(PermissionNotificationsRead)).Post("/notifications/{notificationID}/read", s.handleReadNotification)
			r.With(s.requirePermission(PermissionMetricsRead)).Get("/metrics", s.handleMetrics)
			r.With(s.requireGlobalPermission(PermissionNotificationsManage)).Get("/notification-channels", s.handleListNotificationChannels)
			r.With(s.requireGlobalPermission(PermissionNotificationsManage)).Post("/notification-channels", s.handleCreateNotificationChannel)
			r.With(s.requireGlobalPermission(PermissionNotificationsManage)).Delete("/notification-channels/{channelID}", s.handleDeleteNotificationChannel)
			r.With(s.requireGlobalPermission(PermissionNotificationsManage)).Get("/notification-deliveries", s.handleListNotificationDeliveries)
			r.With(s.requireGlobalPermission(PermissionNotificationsManage)).Get("/notification-deliveries/{deliveryID}/attempts", s.handleNotificationDeliveryAttempts)
			r.With(s.requireGlobalPermission(PermissionNotificationsManage)).Post("/notification-deliveries/{deliveryID}/retry", s.handleRetryNotificationDelivery)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Get("/users", s.handleListUsers)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Post("/users", s.handleCreateUser)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Get("/users/{userID}/role-bindings", s.handleUserRoleBindings)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Patch("/users/{userID}", s.handleUpdateUser)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Post("/users/{userID}/activation", s.handleReissueUserActivation)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Get("/system/updates", s.handleListSystemUpdates)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Post("/system/updates", s.handleCreateSystemUpdate)
			r.With(s.requireInteractiveSession, s.requireGlobalPermission(PermissionRootSecurity)).Get("/system/updates/{jobID}", s.handleGetSystemUpdate)
		})
	})

	staticRoot, _ := fs.Sub(embeddedStatic, "static")
	files := http.FileServer(http.FS(staticRoot))
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") || strings.HasPrefix(req.URL.Path, "/agent/") {
			writeError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		clean := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
		if clean != "." {
			if info, err := fs.Stat(staticRoot, clean); err == nil && !info.IsDir() {
				files.ServeHTTP(w, req)
				return
			}
		}
		body, err := fs.ReadFile(staticRoot, "index.html")
		if err != nil {
			http.Error(w, "UI is not built", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	})
	return r
}

func (s *Server) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		peer := net.ParseIP(host)
		trusted := false
		for _, network := range s.config.TrustedProxyNets {
			if peer != nil && network.Contains(peer) {
				trusted = true
				break
			}
		}
		if trusted {
			forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
			candidate := strings.TrimSpace(forwarded[0])
			if net.ParseIP(candidate) == nil {
				candidate = strings.TrimSpace(r.Header.Get("X-Real-IP"))
			}
			if net.ParseIP(candidate) != nil {
				r.RemoteAddr = candidate
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
			raw := bearerToken(r)
			if raw == "" || !strings.HasPrefix(raw, apiTokenPrefix) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "API token is invalid or expired")
				return
			}
			principal, err := s.store.APITokenPrincipal(r.Context(), tokenHash(raw))
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "API token is invalid or expired")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey, principal)))
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		principal, err := s.store.Session(r.Context(), tokenHash(cookie.Value))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "session is invalid or expired")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey, principal)))
	})
}

func (s *Server) verifyCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalFromContext(r.Context()).IsAPIToken() {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		principal := principalFromContext(r.Context())
		provided := tokenHash(r.Header.Get("X-CSRF-Token"))
		if len(principal.CSRFHash) == 0 || subtle.ConstantTimeCompare(principal.CSRFHash, provided) != 1 {
			writeError(w, http.StatusForbidden, "csrf_failed", "X-CSRF-Token is missing or invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) enforceActivatedAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if principal.User.AccountState != domain.AccountPendingActivation {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/v1/auth/me", "/api/v1/auth/logout", "/api/v1/auth/activate":
			next.ServeHTTP(w, r)
		default:
			writeError(w, http.StatusForbidden, "account_activation_required", "set a new password before using this account")
		}
	})
}

func (s *Server) requirePermission(permission Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !principalFromContext(r.Context()).Allows(permission) {
				writeError(w, http.StatusForbidden, "forbidden", "permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) requireGlobalPermission(permission Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !principalFromContext(r.Context()).GlobalAllows(permission) {
				writeError(w, http.StatusForbidden, "forbidden", "global permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) requireNodePermission(permission Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !principalFromContext(r.Context()).AllowsNodePermission(chi.URLParam(r, "nodeID"), permission) {
				writeError(w, http.StatusForbidden, "forbidden", "node permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) requireInstancePermission(permission Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			instance, err := s.store.GetInstance(r.Context(), chi.URLParam(r, "instanceID"))
			if err != nil {
				s.fail(w, err)
				return
			}
			if !principalFromContext(r.Context()).AllowsInstancePermission(instance, permission) {
				writeError(w, http.StatusForbidden, "forbidden", "instance permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) requireJobPermission(permission Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			job, err := s.store.GetJob(r.Context(), chi.URLParam(r, "jobID"))
			if err != nil {
				s.fail(w, err)
				return
			}
			allowed, _, permissionErr := s.allowsJobPermission(r.Context(), principalFromContext(r.Context()), job, permission)
			if permissionErr != nil {
				s.fail(w, permissionErr)
				return
			}
			if !allowed {
				writeError(w, http.StatusForbidden, "forbidden", "job permission denied")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) requireInteractiveSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalFromContext(r.Context()).IsAPIToken() {
			writeError(w, http.StatusForbidden, "interactive_session_required", "this operation requires a browser session")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func principalFromContext(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalContextKey).(Principal)
	return principal
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database is unavailable")
		return
	}
	if status, err := s.store.SchemaStatusForMigrations(ctx, s.config.MigrationsDir); err != nil || status != "current" {
		writeError(w, http.StatusServiceUnavailable, "schema_unavailable", "database schema is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		return false
	}
	return true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func parseLimit(r *http.Request, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func statusForError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	status, code := statusForError(err)
	message := "request failed"
	if status < 500 {
		message = err.Error()
	} else {
		s.logger.Error("request failed", "error", err)
	}
	writeError(w, status, code, message)
}

func (s *Server) audit(r *http.Request, action, resourceType, resourceID string, metadata map[string]any) {
	principal := principalFromContext(r.Context())
	event := domain.AuditEvent{ActorID: principal.User.ID, ActorName: principal.User.Username, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, RemoteIP: remoteIP(r), Metadata: metadata}
	if err := s.store.WriteAudit(r.Context(), event); err != nil {
		s.logger.Error("write audit event", "action", action, "error", err)
	}
}
