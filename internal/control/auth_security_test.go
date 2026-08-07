package control

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestAPITokenScopesCannotExceedCurrentRole(t *testing.T) {
	if err := validateTokenScopesForRole(domain.RoleViewer, []string{"operate"}); err == nil {
		t.Fatal("viewer was allowed to mint an operate token")
	}
	if err := validateTokenScopesForRole(domain.RoleOperator, []string{"manage"}); err == nil {
		t.Fatal("operator was allowed to mint a manage token")
	}
	if err := validateTokenScopesForRole(domain.RoleAdmin, []string{"manage"}); err != nil {
		t.Fatalf("administrator manage scope was rejected: %v", err)
	}

	principal := Principal{User: domain.User{Role: domain.RoleOwner}, APITokenID: "token", TokenScopes: []string{"read"}}
	if !principal.Allows(PermissionRead) || principal.Allows(PermissionOperate) || principal.Allows(PermissionRootSecurity) {
		t.Fatal("token scope did not narrow the owner's role")
	}
}

func TestAPITokenResourceScope(t *testing.T) {
	nodeToken := Principal{APITokenID: "node-token", TokenNodeID: "node-a"}
	if !nodeToken.AllowsNode("node-a") || nodeToken.AllowsNode("node-b") {
		t.Fatal("node restriction was not enforced")
	}
	if !nodeToken.AllowsInstance(domain.Instance{ID: "one", NodeID: "node-a"}) ||
		nodeToken.AllowsInstance(domain.Instance{ID: "two", NodeID: "node-b"}) {
		t.Fatal("node restriction did not propagate to instances")
	}

	instanceToken := Principal{APITokenID: "instance-token", TokenInstanceID: "one"}
	if instanceToken.AllowsNode("node-a") {
		t.Fatal("instance token gained node-management access")
	}
	if !instanceToken.AllowsJob(domain.Job{InstanceID: "one"}) || instanceToken.AllowsJob(domain.Job{InstanceID: "two"}) {
		t.Fatal("instance restriction did not propagate to jobs")
	}
}

func TestScopedEventFilteringAndPayloadRedaction(t *testing.T) {
	server := &Server{}
	principal := Principal{User: domain.User{Role: domain.RoleAdmin}, APITokenID: "token", TokenScopes: []string{"read"}, TokenNodeID: "node-a"}
	allowed := Event{ResourceType: "instance", ResourceID: "instance-a", Data: map[string]any{
		"instance": map[string]any{"id": "instance-a", "node_id": "node-a"},
		"job": map[string]any{"id": "job-a", "node_id": "node-a", "instance_id": "instance-a",
			"payload": map[string]any{"command": "status"}, "result": map[string]any{"response": "secret"}},
	}}
	if !server.eventAllowed(context.Background(), principal, allowed) {
		t.Fatal("in-scope event was rejected")
	}
	redacted := redactEventForPrincipal(allowed, principal)
	job := redacted.Data["job"].(map[string]any)
	if _, exists := job["payload"]; exists {
		t.Fatal("read-only token received job payload")
	}
	denied := Event{ResourceType: "instance", ResourceID: "instance-b", Data: map[string]any{
		"instance": map[string]any{"id": "instance-b", "node_id": "node-b"},
	}}
	if server.eventAllowed(context.Background(), principal, denied) {
		t.Fatal("out-of-scope event was allowed")
	}
	instancePrincipal := Principal{APITokenID: "instance-token", TokenInstanceID: "instance-a"}
	metric := Event{ResourceType: "node", ResourceID: "node-a", Data: map[string]any{
		"sample": domain.MetricSample{NodeID: "node-a", InstanceID: "instance-a"},
	}}
	if !server.eventAllowed(context.Background(), instancePrincipal, metric) {
		t.Fatal("instance token did not receive its own metric event")
	}
	metric.Data["sample"] = domain.MetricSample{NodeID: "node-a", InstanceID: "instance-b"}
	if server.eventAllowed(context.Background(), instancePrincipal, metric) {
		t.Fatal("instance token received another instance's metric event")
	}
}

func TestScopedLiveLogAuthorizationUsesEmbeddedOwnershipAndFailsClosed(t *testing.T) {
	t.Parallel()
	server := &Server{}
	principal := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{
		{Role: domain.RoleViewer, ScopeType: "node", NodeID: "node-a"},
	}}
	logEvent := Event{Type: "instance.log", ResourceType: "instance", ResourceID: "instance-a", Data: map[string]any{
		"node_id": "node-a", "instance_id": "instance-a", "line": "ready",
	}}
	if !server.eventAllowed(context.Background(), principal, logEvent) {
		t.Fatal("node-scoped viewer did not receive an embedded in-scope log event")
	}
	logEvent.Data["node_id"] = "node-b"
	if server.eventAllowed(context.Background(), principal, logEvent) {
		t.Fatal("node-scoped viewer received a cross-node log event")
	}
	delete(logEvent.Data, "node_id")
	if server.eventAllowed(context.Background(), principal, logEvent) {
		t.Fatal("ownership-free log event did not fail closed")
	}

	instancePrincipal := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{
		{Role: domain.RoleViewer, ScopeType: "instance", InstanceID: "instance-a"},
	}}
	if !server.eventAllowed(context.Background(), instancePrincipal, logEvent) {
		t.Fatal("direct instance binding could not authorize its own compact event")
	}
}

func TestGlobalBindingEventsStillRequireConcretePermission(t *testing.T) {
	t.Parallel()
	server := &Server{}
	principal := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{{
		Role:        domain.RoleAdmin,
		ScopeType:   "global",
		Permissions: []string{string(PermissionInstancesRead), string(PermissionBackupsCreate)},
	}}}
	instance := Event{Type: "instance.updated", ResourceType: "instance", ResourceID: "instance-a", Data: map[string]any{
		"instance": map[string]any{"id": "instance-a", "node_id": "node-a"},
	}}
	if !server.eventAllowed(context.Background(), principal, instance) {
		t.Fatal("explicit instance read permission did not allow instance event")
	}
	logEvent := Event{Type: "instance.log", ResourceType: "instance", ResourceID: "instance-a", Data: map[string]any{
		"node_id": "node-a", "instance_id": "instance-a", "line": "secret",
	}}
	if server.eventAllowed(context.Background(), principal, logEvent) {
		t.Fatal("global binding received log event without logs.read")
	}
	job := Event{Type: "job.updated", ResourceType: "job", ResourceID: "job-a", Data: map[string]any{
		"job": map[string]any{"id": "job-a", "node_id": "node-a", "instance_id": "instance-a"},
	}}
	if server.eventAllowed(context.Background(), principal, job) {
		t.Fatal("global binding received job event without jobs.read")
	}
}

func TestEventMapRedactionUsesJobResourcePermission(t *testing.T) {
	t.Parallel()
	principal := Principal{BindingsLoaded: true, Bindings: []domain.RoleBinding{
		{Role: domain.RoleOperator, ScopeType: "instance", InstanceID: "instance-a"},
		{Role: domain.RoleViewer, ScopeType: "instance", InstanceID: "instance-b"},
	}}
	event := Event{ResourceType: "job", ResourceID: "job-b", Data: map[string]any{"job": map[string]any{
		"id": "job-b", "node_id": "node-b", "instance_id": "instance-b",
		"payload": map[string]any{"command": "status"}, "result": map[string]any{"response": "sensitive"},
	}}}
	redacted := redactEventForPrincipal(event, principal)
	job := redacted.Data["job"].(map[string]any)
	if _, exists := job["payload"]; exists {
		t.Fatal("viewer received job payload through map-backed event because of operate permission elsewhere")
	}
	if _, exists := job["result"]; exists {
		t.Fatal("viewer received job result through map-backed event because of operate permission elsewhere")
	}
}

func TestRecoveryCodesAreUniqueNormalizedAndHashed(t *testing.T) {
	codes, digests, err := newTOTPRecoveryCodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 || len(digests) != 10 {
		t.Fatalf("got %d codes and %d digests", len(codes), len(digests))
	}
	seen := make(map[string]bool)
	for index, code := range codes {
		normalized := normalizeRecoveryCode(code)
		if len(normalized) != 16 || seen[normalized] {
			t.Fatalf("invalid or duplicate recovery code %q", code)
		}
		seen[normalized] = true
		if !bytes.Equal(digests[index], recoveryCodeHash(normalized)) {
			t.Fatal("stored digest does not match the normalized code")
		}
	}
	if normalizeRecoveryCode(strings.ToLower(codes[0])) != normalizeRecoveryCode(codes[0]) {
		t.Fatal("recovery code normalization is not case insensitive")
	}
}

func TestLoginRequestAcceptsExactlyOneSecondFactor(t *testing.T) {
	base := loginRequest{Username: "owner", Password: "password"}
	withRecovery := base
	withRecovery.RecoveryCode = "ABCD-EFGH-JKLM-NPQR"
	if err := validateLoginRequest(withRecovery); err != nil {
		t.Fatalf("valid recovery code was rejected: %v", err)
	}
	withBoth := withRecovery
	withBoth.TOTPCode = "123456"
	if err := validateLoginRequest(withBoth); err == nil {
		t.Fatal("request with two second factors was accepted")
	}
	withRecovery.RecoveryCode = "not-a-code"
	if err := validateLoginRequest(withRecovery); err == nil {
		t.Fatal("malformed recovery code was accepted")
	}
}

func TestAPITokenSkipsCSRFBecauseItIsNotCookieAuthenticated(t *testing.T) {
	server := &Server{}
	handler := server.verifyCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{APITokenID: "token"}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("API token was subjected to CSRF validation: %d", response.Code)
	}
}

func TestCreateAPITokenRequiresExpiryBeforeStoreAccess(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/api-tokens",
		strings.NewReader(`{"name":"automation","scopes":["read"]}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
		User: domain.User{Role: domain.RoleOwner},
	}))
	response := httptest.NewRecorder()
	server.handleCreateAPIToken(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "expires_at is required") {
		t.Fatalf("missing expiry returned %d: %s", response.Code, response.Body.String())
	}

	expired := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/api-tokens",
		strings.NewReader(`{"name":"automation","scopes":["read"],"expires_at":"`+expired+`"}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
		User: domain.User{Role: domain.RoleOwner},
	}))
	response = httptest.NewRecorder()
	server.handleCreateAPIToken(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expired token request returned %d", response.Code)
	}
}
