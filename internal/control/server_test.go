package control

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestRealIPOnlyTrustsConfiguredProxy(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	server := &Server{config: Config{TrustedProxyNets: []*net.IPNet{trusted}}}
	handler := server.realIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(remoteIP(r)))
	}))

	request := httptest.NewRequest(http.MethodGet, "http://panel.test/", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.5")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Body.String() != "192.0.2.10" {
		t.Fatalf("untrusted proxy spoofed address: %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "http://panel.test/", nil)
	request.RemoteAddr = "10.1.2.3:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.5")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Body.String() != "203.0.113.5" {
		t.Fatalf("trusted proxy address was ignored: %s", response.Body.String())
	}
}

func TestSecurityHeadersRestrictConnectionsAndDisableAPICaching(t *testing.T) {
	server := &Server{}
	handler := server.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "https://panel.test/api/v1/auth/me", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	csp := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "connect-src 'self' ws:") || !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("API response cache policy = %q", response.Header().Get("Cache-Control"))
	}
}

func TestAbortBackupUploadValidatesKeyAndEmptyBodyBeforeStore(t *testing.T) {
	server := &Server{}
	tests := []struct {
		name string
		body string
		key  string
		code string
	}{
		{name: "missing key", code: "idempotency_key_required"},
		{name: "oversized key", key: strings.Repeat("x", 201), code: "idempotency_key_required"},
		{name: "unexpected body", key: "abort-key", body: `{}`, code: "invalid_backup_upload_abort"},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodDelete, "/api/v1/instances/instance/backup-uploads/upload", strings.NewReader(item.body))
			if item.key != "" {
				request.Header.Set("Idempotency-Key", item.key)
			}
			response := httptest.NewRecorder()
			server.handleAbortBackupUpload(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), item.code) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestOperatorCannotSubmitManageInstanceActions(t *testing.T) {
	server := &Server{}
	actions := []string{
		"install_package", "toggle_package",
	}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/actions",
				strings.NewReader(`{"type":"`+action+`","payload":{}}`))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
				User: domain.User{ID: "operator", Role: domain.RoleOperator},
			}))
			response := httptest.NewRecorder()

			server.handleInstanceAction(response, request)

			if response.Code != http.StatusForbidden {
				t.Fatalf("operator action %q returned %d, want 403: %s", action, response.Code, response.Body.String())
			}
		})
	}
}

func TestGenericInstanceActionContractIsSeparateFromInternalJobs(t *testing.T) {
	publicActions := map[string]Permission{
		"backup":          PermissionBackupsCreate,
		"ban_player":      PermissionPlayersBan,
		"broadcast":       PermissionPlayersBroadcast,
		"change_map":      PermissionPlayersChangeMap,
		"create_schedule": PermissionSchedulesManage,
		"install_package": PermissionPackagesInstall,
		"kick_player":     PermissionPlayersKick,
		"rcon":            PermissionRCONExecute,
		"restart":         PermissionInstancesRestart,
		"restore":         PermissionBackupsRestore,
		"start":           PermissionInstancesStart,
		"stop":            PermissionInstancesStop,
		"toggle_package":  PermissionPackagesToggle,
		"unban_player":    PermissionPlayersUnban,
	}
	if len(genericInstanceActionPermissions) != len(publicActions) {
		t.Fatalf("generic action allowlist has %d entries, want %d", len(genericInstanceActionPermissions), len(publicActions))
	}
	for action, wantPermission := range publicActions {
		if !genericInstanceActionAllowed(action) {
			t.Errorf("documented generic action %q is not allowed", action)
			continue
		}
		if permission, ok := genericInstanceActionPermission(action); !ok || permission != wantPermission {
			t.Errorf("generic action %q permission = %q, %v; want %q", action, permission, ok, wantPermission)
		}
	}

	internalOnly := []string{
		"a2s.info", "a2s.players", "backup.chunk", "backup.upload.abort", "configure", "create", "delete", "detach", "file", "game.import",
		"import", "install", "install_mod", "list", "logs.chunk", "logs.search", "rcon.players", "reinstall", "rollback_package", "update", "validate",
	}
	for _, action := range internalOnly {
		if genericInstanceActionAllowed(action) {
			t.Errorf("internal job %q leaked into the generic endpoint allowlist", action)
		}
		if _, ok := InstanceActionPermission(action); !ok {
			t.Errorf("internal job %q lost its permission mapping", action)
		}
	}
	for _, alias := range []string{"broadcast", "create_schedule", "install_package"} {
		if _, ok := InstanceActionPermission(alias); ok {
			t.Errorf("public alias %q must not be accepted as an internal job type", alias)
		}
	}
}

func TestGameInstallationActionContractRetiresReinstall(t *testing.T) {
	for _, action := range []string{"install", "import", "validate", "update"} {
		if !gameInstallationActionAllowed(action) {
			t.Errorf("supported game installation action %q was rejected", action)
		}
		if !environmentJobTypes["game."+action] {
			t.Errorf("supported game installation job %q cannot be queued", action)
		}
	}
	if gameInstallationActionAllowed("reinstall") {
		t.Fatal("retired reinstall action remains in the public handler contract")
	}
	if environmentJobTypes["game.reinstall"] {
		t.Fatal("retired game.reinstall task can still be queued by current Control code")
	}
	if definition := permissionDefinitions[PermissionEnvironmentsManage]; definition.Class != PermissionManage || definition.Scope != permissionScopeNode {
		t.Fatalf("remaining maintenance permission=%#v, want node-scoped manage", definition)
	}
}

func TestBroadcastOnlyRequiresDedicatedBroadcastPermission(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "broadcast-permission.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	const nodeID = "4fe0dd94-ce80-448c-b9fa-20a2dccbfa44"
	const instanceID = "3303620c-8c20-47c3-8c67-e49560416a3f"
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO nodes(id,name,status) VALUES(?1,'broadcast-node','online');
		INSERT INTO instances(id,node_id,name,status,port,config) VALUES(?2,?1,'broadcast-instance','stopped',27015,'{}');
	`, nodeID, instanceID); err != nil {
		t.Fatal(err)
	}

	principal := Principal{User: owner, BindingsLoaded: true, Bindings: []domain.RoleBinding{{
		Role: domain.RoleOperator, ScopeType: "instance", InstanceID: instanceID,
		Permissions: []string{string(PermissionInstancesRead), string(PermissionPlayersBroadcast)},
	}}}
	instance := domain.Instance{ID: instanceID, NodeID: nodeID}
	if !principal.AllowsInstancePermission(instance, PermissionPlayersBroadcast) {
		t.Fatal("test principal is missing broadcast permission")
	}
	if principal.AllowsInstancePermission(instance, PermissionRCONExecute) {
		t.Fatal("test principal unexpectedly has arbitrary RCON permission")
	}
	if permission, ok := normalizedInstanceActionPermission("broadcast", "rcon"); !ok || permission != PermissionPlayersBroadcast {
		t.Fatalf("normalized broadcast permission=%q, %v", permission, ok)
	}

	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/"+instanceID+"/actions",
		strings.NewReader(`{"type":"broadcast","payload":{"message":"Maintenance soon"}}`))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("instanceID", instanceID)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, principal))
	response := httptest.NewRecorder()

	server.handleInstanceAction(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("broadcast returned %d, want 202: %s", response.Code, response.Body.String())
	}
	var jobType string
	if err := store.db.QueryRowContext(ctx, `SELECT type FROM jobs WHERE instance_id=?1 ORDER BY created_at DESC LIMIT 1`, instanceID).Scan(&jobType); err != nil {
		t.Fatal(err)
	}
	if jobType != "rcon" {
		t.Fatalf("broadcast queued job type %q, want fixed rcon job", jobType)
	}
}

func TestRetiredPermissionsAreNotAssignable(t *testing.T) {
	for _, permission := range []Permission{"instances.update", "instances.validate", "files.upload"} {
		if _, ok := permissionClass(permission); ok {
			t.Errorf("retired permission %q remains recognized", permission)
		}
	}
	for _, action := range []string{"update", "validate"} {
		if permission, ok := InstanceActionPermission(action); !ok || permission != PermissionEnvironmentsManage {
			t.Errorf("legacy internal job %q permission=%q, %v; want %q", action, permission, ok, PermissionEnvironmentsManage)
		}
		if err := validateScheduledPayload(action, map[string]any{}); err == nil {
			t.Errorf("retired scheduled action %q was accepted", action)
		}
	}
}

func TestInternalJobsCannotBeSubmittedThroughGenericActions(t *testing.T) {
	for _, action := range []string{
		"a2s.info", "a2s.players", "backup.chunk", "backup.upload.abort", "configure", "create", "file", "game.import", "import", "install",
		"install_mod", "list", "logs.chunk", "logs.search", "rcon.players",
	} {
		t.Run(action, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/actions",
				strings.NewReader(`{"type":"`+action+`","payload":{}}`))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
				User: domain.User{ID: "owner", Role: domain.RoleOwner},
			}))
			response := httptest.NewRecorder()

			server.handleInstanceAction(response, request)

			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unsupported instance action") {
				t.Fatalf("internal action %q returned %d: %s", action, response.Code, response.Body.String())
			}
		})
	}
}

func TestDeleteCannotBeSubmittedOrScheduledThroughGenericActions(t *testing.T) {
	for _, action := range []string{"delete", "detach"} {
		t.Run(action, func(t *testing.T) {
			if genericInstanceActionAllowed(action) {
				t.Fatalf("%s remained in the generic endpoint allowlist", action)
			}
			if permission, allowed := InstanceActionPermission(action); !allowed || permission != PermissionInstancesDelete {
				t.Fatalf("dedicated %s endpoint lost its manage permission", action)
			}
			server := &Server{}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/actions",
				strings.NewReader(`{"type":"`+action+`","payload":{}}`))
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
				User: domain.User{ID: "owner", Role: domain.RoleOwner},
			}))
			response := httptest.NewRecorder()
			server.handleInstanceAction(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "DELETE /api/v1/instances") {
				t.Fatalf("generic %s returned %d: %s", action, response.Code, response.Body.String())
			}
		})
	}

	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/actions",
		strings.NewReader(`{"type":"create_schedule","payload":{"name":"unsafe","cron":"0 4 * * *","action":"delete"}}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
		User: domain.User{ID: "owner", Role: domain.RoleOwner},
	}))
	response := httptest.NewRecorder()

	server.handleInstanceAction(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unsupported scheduled action") {
		t.Fatalf("scheduled delete returned %d: %s", response.Code, response.Body.String())
	}
}

func TestViewerJobRedaction(t *testing.T) {
	job := domain.Job{Type: "rcon", Payload: map[string]any{"command": "status", "command_ciphertext": "sealed-command"},
		Result: map[string]any{"response": encryptedValuePlaceholder, "response_ciphertext": "sealed-response",
			terminalOutcomeFingerprintField: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			jobProjectionPendingField:       true}}
	redacted := redactJob(job, domain.RoleViewer)
	if redacted.Payload != nil || redacted.Result != nil {
		t.Fatal("viewer received job payload or result")
	}
	operator := redactJob(job, domain.RoleOperator)
	if operator.Payload == nil || operator.Result == nil {
		t.Fatal("operator job detail was unexpectedly redacted")
	}
	if operator.Payload["command_ciphertext"] != nil || operator.Result["response_ciphertext"] != nil ||
		operator.Result[terminalOutcomeFingerprintField] != nil || operator.Result[jobProjectionPendingField] != nil {
		t.Fatalf("operator response exposed internal ciphertext or terminal fingerprint: %#v %#v", operator.Payload, operator.Result)
	}
}

func TestJobRedactionUsesTargetResourcePermission(t *testing.T) {
	t.Parallel()
	job := domain.Job{
		NodeID:     "node-b",
		InstanceID: "instance-b",
		Payload:    map[string]any{"command": "status"},
		Result:     map[string]any{"response": "sensitive output"},
	}
	principal := Principal{
		User:           domain.User{ID: "mixed-role"},
		BindingsLoaded: true,
		Bindings: []domain.RoleBinding{
			{Role: domain.RoleOperator, ScopeType: "instance", InstanceID: "instance-a", Permissions: presetPermissionsForScope(domain.RoleOperator, "instance")},
			{Role: domain.RoleViewer, ScopeType: "instance", InstanceID: "instance-b", Permissions: presetPermissionsForScope(domain.RoleViewer, "instance")},
		},
	}
	redacted := redactJobForPrincipal(job, principal)
	if redacted.Payload != nil || redacted.Result != nil {
		t.Fatal("viewer received job payload or result because of operate permission on another resource")
	}

	principal.Bindings[1].Role = domain.RoleOperator
	principal.Bindings[1].Permissions = presetPermissionsForScope(domain.RoleOperator, "instance")
	visible := redactJobForPrincipal(job, principal)
	if visible.Payload == nil || visible.Result == nil {
		t.Fatal("target-resource operator job detail was unexpectedly redacted")
	}
}

func TestValidateImportLocationRequiresAbsolutePath(t *testing.T) {
	valid := []string{"/srv/l4d2/server-one", `C:\servers\existing\l4d2`}
	for _, path := range valid {
		if err := validateImportLocation(path); err != nil {
			t.Errorf("valid import path %q was rejected: %v", path, err)
		}
	}
	invalid := []string{"", "server-one", "../outside", "/srv/../outside", `C:\servers\..\outside`, `\\server\share`}
	for _, path := range invalid {
		if err := validateImportLocation(path); err == nil {
			t.Errorf("unsafe import path %q was accepted", path)
		}
	}
	if err := validateImportLocation("/srv/l4d2/server"); err != nil {
		t.Fatalf("absolute import path was rejected: %v", err)
	}
}
