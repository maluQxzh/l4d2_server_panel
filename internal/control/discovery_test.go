package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestNormalizeDiscoveryRequestBounds(t *testing.T) {
	request, err := normalizeDiscoveryRequest(discoveryRequest{})
	if err != nil || request.MaxDepth != 6 || request.MaxEntries != 5000 {
		t.Fatalf("defaults = %#v, err = %v", request, err)
	}
	for _, request := range []discoveryRequest{{MaxDepth: 9}, {MaxEntries: 20001}, {MaxDepth: -1}, {MaxEntries: -1}} {
		if _, err := normalizeDiscoveryRequest(request); err == nil {
			t.Fatalf("expected %#v to be rejected", request)
		}
	}
}

func TestFixedDiscoveryJobResultValidatesCandidatePaths(t *testing.T) {
	t.Parallel()
	resultForPath := func(candidatePath string) map[string]any {
		candidateName := candidatePath
		if candidatePath == "." {
			candidateName = "Server root"
		} else if separator := strings.LastIndex(candidatePath, "/"); separator >= 0 {
			candidateName = candidatePath[separator+1:]
		}
		return map[string]any{
			"candidates": []map[string]any{{
				"root_index": 0,
				"path":       candidatePath,
				"name":       candidateName,
				"executable": "srcds.exe",
				"evidence":   []string{"srcds.exe", "left4dead2/"},
			}},
			"scanned_directories": 1,
			"skipped_links":       0,
			"truncated":           false,
		}
	}
	for _, candidatePath := range []string{".", "server", "servers/l4d2"} {
		candidatePath := candidatePath
		t.Run("valid "+candidatePath, func(t *testing.T) {
			t.Parallel()
			if _, err := fixedDiscoveryJobResult(domain.Job{Status: domain.JobSucceeded, Result: resultForPath(candidatePath)}); err != nil {
				t.Fatalf("valid candidate path %q was rejected: %v", candidatePath, err)
			}
		})
	}
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "absolute", path: "/srv/l4d2"},
		{name: "drive", path: "C:/Servers/l4d2"},
		{name: "drive relative", path: "C:Servers/l4d2"},
		{name: "UNC slash", path: "//server/share"},
		{name: "UNC backslash", path: `\\server\share`},
		{name: "backslash", path: `servers\l4d2`},
		{name: "parent", path: "../l4d2"},
		{name: "nested parent", path: "servers/../l4d2"},
		{name: "dot component", path: "servers/./l4d2"},
		{name: "reserved device", path: "servers/CON"},
		{name: "reserved device extension", path: "servers/NUL.txt"},
		{name: "alternate stream", path: "servers/l4d2:secret"},
		{name: "trailing dot", path: "servers/l4d2."},
		{name: "trailing space", path: "servers/l4d2 "},
	} {
		test := test
		t.Run("reject "+test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := fixedDiscoveryJobResult(domain.Job{Status: domain.JobSucceeded, Result: resultForPath(test.path)}); err == nil {
				t.Fatalf("unsafe candidate path %q was accepted", test.path)
			}
		})
	}
	for _, executable := range []string{"srcds.exe", "srcds_run", "srcds_linux"} {
		executable := executable
		t.Run("valid executable "+executable, func(t *testing.T) {
			t.Parallel()
			result := resultForPath("server")
			candidate := result["candidates"].([]map[string]any)[0]
			candidate["executable"] = executable
			candidate["evidence"] = []string{executable, "left4dead2/"}
			if _, err := fixedDiscoveryJobResult(domain.Job{Status: domain.JobSucceeded, Result: result}); err != nil {
				t.Fatalf("fixed executable %q was rejected: %v", executable, err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "absolute candidate name",
			mutate: func(candidate map[string]any) {
				candidate["name"] = `C:\secrets\server`
			},
		},
		{
			name: "secret candidate name mismatches path",
			mutate: func(candidate map[string]any) {
				candidate["name"] = "node-credential-secret"
			},
		},
		{
			name: "absolute executable",
			mutate: func(candidate map[string]any) {
				candidate["executable"] = `C:\Servers\srcds.exe`
			},
		},
		{
			name: "secret executable",
			mutate: func(candidate map[string]any) {
				candidate["executable"] = "node-credential-secret"
			},
		},
		{
			name: "absolute evidence",
			mutate: func(candidate map[string]any) {
				candidate["evidence"] = []string{"srcds.exe", `C:\secrets\gameinfo.txt`}
			},
		},
		{
			name: "secret evidence",
			mutate: func(candidate map[string]any) {
				candidate["evidence"] = []string{"srcds.exe", "node-credential-secret"}
			},
		},
	} {
		test := test
		t.Run("reject "+test.name, func(t *testing.T) {
			t.Parallel()
			result := resultForPath("server")
			test.mutate(result["candidates"].([]map[string]any)[0])
			if _, err := fixedDiscoveryJobResult(domain.Job{Status: domain.JobSucceeded, Result: result}); err == nil {
				t.Fatal("unsafe discovery metadata was accepted")
			}
		})
	}
	t.Run("messages and root names are canonicalized", func(t *testing.T) {
		t.Parallel()
		result := resultForPath(".")
		result["candidates"].([]map[string]any)[0]["name"] = "node-credential-secret"
		fixed, err := fixedDiscoveryJobResult(domain.Job{
			Status:  domain.JobSucceeded,
			Message: `failed near C:\secrets\server`,
			Result:  result,
		})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(fixed)
		if err != nil {
			t.Fatal(err)
		}
		if fixed.Message != "" || strings.Contains(string(encoded), "node-credential-secret") || strings.Contains(string(encoded), "secrets") {
			t.Fatalf("fixed discovery result leaked Agent metadata: %s", encoded)
		}
	})
	t.Run("failed job message is cleared", func(t *testing.T) {
		t.Parallel()
		fixed, err := fixedDiscoveryJobResult(domain.Job{
			Status:      domain.JobFailed,
			Message:     `failed near C:\secrets\server`,
			Payload:     map[string]any{"secret": "payload-secret"},
			Result:      map[string]any{"secret": "result-secret"},
			RequestedBy: "requester-secret",
		})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(fixed)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secrets", "payload-secret", "result-secret", "requester-secret"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("failed discovery result leaked %q: %s", secret, encoded)
			}
		}
	})
}

func TestExternalInstanceJobAllowlist(t *testing.T) {
	allowed := []struct {
		action  string
		payload map[string]any
	}{
		{"restart", nil}, {"a2s.info", nil}, {"list", nil}, {"backup", nil},
		{"file", map[string]any{"operation": "read"}}, {"delete", map[string]any{"mode": "detach"}},
	}
	for _, test := range allowed {
		if !externalInstanceJobAllowed(test.action, test.payload) {
			t.Errorf("expected %s %#v to be allowed", test.action, test.payload)
		}
	}
	denied := []struct {
		action  string
		payload map[string]any
	}{
		{"start", nil}, {"stop", nil}, {"update", nil}, {"configure", nil},
		{"file", map[string]any{"operation": "write"}}, {"delete", map[string]any{"mode": "quarantine"}},
	}
	for _, test := range denied {
		if externalInstanceJobAllowed(test.action, test.payload) {
			t.Errorf("expected %s %#v to be denied", test.action, test.payload)
		}
	}
}

func TestNodeDiscoveryPollingOnlyRequiresNodeDiscoveryPermission(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "discovery-permission.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state)
			VALUES('discover-user','discover-user','discover-user','Discover user','hash','admin','active');
		INSERT INTO nodes(id,name,status) VALUES
			('node-a','Node A','online'),('node-b','Node B','online'),('node-c','Node C','online');
		INSERT INTO role_bindings(id,user_id,role,scope_type,node_id) VALUES
			('discover-node-a','discover-user','admin','node','node-a'),
			('discover-node-b','discover-user','admin','node','node-b');
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES
			('discover-node-a','nodes.read'),('discover-node-a','nodes.discover'),
			('discover-node-b','nodes.read'),('discover-node-b','nodes.discover');
		INSERT INTO jobs(id,node_id,type,status,progress,message,payload,result,requested_by) VALUES(
			'discovery-job','node-a','discover_instances','succeeded',100,'C:/secret-message',
			'{"max_depth":6,"internal_secret":"do-not-return"}',
			'{"candidates":[{"root_index":0,"path":"server","name":"server","executable":"srcds.exe","has_gameinfo":true,"has_appmanifest":true,"managed":false,"likely_running":false,"evidence":["srcds.exe","left4dead2/","left4dead2/gameinfo.txt","appmanifest_222860.acf"],"absolute_path":"C:/secret"}],"scanned_directories":12,"skipped_links":1,"truncated":false,"unexpected_secret":"do-not-return"}',
			'discover-user');
	`); err != nil {
		t.Fatal(err)
	}
	const sessionSecret = "discovery-session-secret"
	if _, err := store.CreateSession(ctx, "discover-user", tokenHash(sessionSecret), tokenHash("csrf"), "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	principal, err := store.Session(ctx, tokenHash(sessionSecret))
	if err != nil {
		t.Fatal(err)
	}
	if principal.Allows(PermissionJobsRead) || principal.Allows(PermissionJobsDetails) {
		t.Fatal("discovery-only principal unexpectedly has generic job permissions")
	}

	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	requestDiscovery := func(nodeID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+nodeID+"/discoveries/discovery-job", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionSecret})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	response := requestDiscovery("node-a")
	if response.Code != http.StatusOK {
		t.Fatalf("in-scope discovery poll returned %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Job domain.Job `json:"job"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	candidates, ok := body.Job.Result["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("discovery candidates=%#v", body.Job.Result["candidates"])
	}
	encoded := response.Body.String()
	for _, secret := range []string{"internal_secret", "unexpected_secret", "absolute_path", "do-not-return", "C:/secret", "secret-message"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("discovery response leaked %q: %s", secret, encoded)
		}
	}
	if response := requestDiscovery("node-b"); response.Code != http.StatusNotFound {
		t.Fatalf("cross-node job lookup returned %d, want 404: %s", response.Code, response.Body.String())
	}
	if response := requestDiscovery("node-c"); response.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope node lookup returned %d, want 403: %s", response.Code, response.Body.String())
	}
}
