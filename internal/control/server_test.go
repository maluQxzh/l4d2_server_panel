package control

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		"backup":          PermissionOperate,
		"ban_player":      PermissionOperate,
		"broadcast":       PermissionOperate,
		"change_map":      PermissionOperate,
		"create_schedule": PermissionOperate,
		"install_package": PermissionManage,
		"kick_player":     PermissionOperate,
		"rcon":            PermissionOperate,
		"restart":         PermissionOperate,
		"restore":         PermissionOperate,
		"start":           PermissionOperate,
		"stop":            PermissionOperate,
		"toggle_package":  PermissionManage,
		"unban_player":    PermissionOperate,
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
			if permission, allowed := InstanceActionPermission(action); !allowed || permission != PermissionManage {
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
			{Role: domain.RoleOperator, ScopeType: "instance", InstanceID: "instance-a"},
			{Role: domain.RoleViewer, ScopeType: "instance", InstanceID: "instance-b"},
		},
	}
	redacted := redactJobForPrincipal(job, principal)
	if redacted.Payload != nil || redacted.Result != nil {
		t.Fatal("viewer received job payload or result because of operate permission on another resource")
	}

	principal.Bindings[1].Role = domain.RoleOperator
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
