package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestInstanceScopedMapJobsAreVisibleAndIsolatedBeforeLimit(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "scoped-map-jobs.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state) VALUES
			('map-user','map-user','map-user','Map user','hash','operator','active'),
			('other-user','other-user','other-user','Other user','hash','operator','active');
		INSERT INTO nodes(id,name,status) VALUES('map-node','Map node','online');
		INSERT INTO instances(id,node_id,name,status,port,config) VALUES
			('instance-a','map-node','Instance A','stopped',27015,'{}'),
			('instance-b','map-node','Instance B','stopped',27016,'{}');
		INSERT INTO role_bindings(id,user_id,role,scope_type,instance_id) VALUES
			('binding-a','map-user','operator','instance','instance-a'),
			('binding-b','other-user','operator','instance','instance-b');
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES
			('binding-a','instances.read'),('binding-a','jobs.read'),('binding-a','jobs.details'),('binding-a','jobs.cancel'),
			('binding-b','instances.read'),('binding-b','jobs.read'),('binding-b','jobs.details'),('binding-b','jobs.cancel');
		INSERT INTO jobs(id,node_id,type,status,progress,payload,result,requested_by,created_at) VALUES
			('revoked-scope-job','map-node','map.verify','failed',100,'{"map_id":"map-b","_scope_instance_id":"instance-b"}','{}','map-user','2025-01-03T00:00:00Z'),
			('scoped-map-job','map-node','map.verify','queued',0,'{"map_id":"map-a","_scope_instance_id":"instance-a"}','{}','map-user','2025-01-02T00:00:00Z'),
			('legacy-map-job','map-node','map.import.qq_flash','failed',100,'{"map_id":"map-a"}','{"error":"invalid VPK"}','map-user','2025-01-01T00:00:00Z');
	`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 120; index++ {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,progress,payload,result,requested_by,created_at)
			VALUES(?1,'map-node','instance-b','restart','succeeded',100,'{}','{}','other-user','2026-01-01T00:00:00Z')`, fmt.Sprintf("other-job-%03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	createSession := func(userID, secret string) {
		t.Helper()
		if _, err := store.CreateSession(ctx, userID, tokenHash(secret), tokenHash("csrf-"+userID), "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	createSession("map-user", "map-session")
	createSession("other-user", "other-session")

	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	requestAs := func(method, path, session, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, bytes.NewReader(nil))
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		if csrf != "" {
			request.Header.Set("X-CSRF-Token", csrf)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	listResponse := requestAs(http.MethodGet, "/api/v1/jobs?background=exclude&limit=2", "map-session", "")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("scoped list=%d %s", listResponse.Code, listResponse.Body.String())
	}
	var listBody struct {
		Items []domain.Job `json:"items"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if len(listBody.Items) != 2 || listBody.Items[0].ID != "scoped-map-job" || listBody.Items[1].ID != "legacy-map-job" {
		t.Fatalf("scoped list did not filter before limit: %#v", listBody.Items)
	}
	if listBody.Items[0].ScopeInstanceID != "instance-a" || listBody.Items[1].ScopeInstanceID != "instance-a" {
		t.Fatalf("missing persisted/legacy instance scopes: %#v", listBody.Items)
	}
	if _, exists := listBody.Items[0].Payload[jobScopeInstancePayloadField]; exists {
		t.Fatalf("internal scope leaked in job payload: %#v", listBody.Items[0].Payload)
	}

	if response := requestAs(http.MethodGet, "/api/v1/jobs/scoped-map-job", "map-session", ""); response.Code != http.StatusOK {
		t.Fatalf("scoped job details=%d %s", response.Code, response.Body.String())
	}
	if response := requestAs(http.MethodPost, "/api/v1/jobs/scoped-map-job/cancel", "map-session", "csrf-map-user"); response.Code != http.StatusOK {
		t.Fatalf("scoped job cancel=%d %s", response.Code, response.Body.String())
	}
	for _, jobID := range []string{"scoped-map-job", "legacy-map-job"} {
		if response := requestAs(http.MethodGet, "/api/v1/jobs/"+jobID, "other-session", ""); response.Code != http.StatusForbidden {
			t.Fatalf("cross-instance job %s response=%d, want 403: %s", jobID, response.Code, response.Body.String())
		}
	}
	if response := requestAs(http.MethodGet, "/api/v1/jobs/revoked-scope-job", "map-session", ""); response.Code != http.StatusForbidden {
		t.Fatalf("persisted scope was overridden by legacy compatibility: %d %s", response.Code, response.Body.String())
	}
}

func TestJobDetailsPermissionIsIndependentFromJobRead(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "job-permissions.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state) VALUES
			('details-user','details-user','details-user','Details user','hash','operator','active'),
			('read-user','read-user','read-user','Read user','hash','viewer','active'),
			('other-user','other-user','other-user','Other user','hash','operator','active');
		INSERT INTO nodes(id,name,status) VALUES('job-node-a','Job node A','online'),('job-node-b','Job node B','online');
		INSERT INTO role_bindings(id,user_id,role,scope_type,node_id) VALUES
			('details-binding','details-user','operator','node','job-node-a'),
			('read-binding','read-user','viewer','node','job-node-a'),
			('other-binding','other-user','operator','node','job-node-b');
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES
			('details-binding','nodes.read'),('details-binding','jobs.details'),
			('read-binding','nodes.read'),('read-binding','jobs.read'),
			('other-binding','nodes.read'),('other-binding','jobs.details');
		INSERT INTO jobs(id,node_id,type,status,progress,payload,result,requested_by) VALUES(
			'job-a','job-node-a','rcon','succeeded',100,
			'{"command":"status","command_ciphertext":"sealed-command"}',
			'{"response":"safe response","response_ciphertext":"sealed-response"}',
			'details-user');
	`); err != nil {
		t.Fatal(err)
	}
	createSession := func(userID, secret string) {
		t.Helper()
		if _, err := store.CreateSession(ctx, userID, tokenHash(secret), tokenHash("csrf-"+userID), "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	createSession("details-user", "details-session")
	createSession("read-user", "read-session")
	createSession("other-user", "other-session")

	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	requestAs := func(path, session string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	decodeJob := func(response *httptest.ResponseRecorder) domain.Job {
		t.Helper()
		var body struct {
			Job domain.Job `json:"job"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Job
	}

	detailsResponse := requestAs("/api/v1/jobs/job-a", "details-session")
	if detailsResponse.Code != http.StatusOK {
		t.Fatalf("details-only job response=%d %s", detailsResponse.Code, detailsResponse.Body.String())
	}
	detailsJob := decodeJob(detailsResponse)
	if detailsJob.Payload["command"] != "status" || detailsJob.Result["response"] != "safe response" {
		t.Fatalf("details-only job body=%#v %#v", detailsJob.Payload, detailsJob.Result)
	}
	if detailsJob.Payload["command_ciphertext"] != nil || detailsJob.Result["response_ciphertext"] != nil {
		t.Fatalf("details-only job leaked ciphertext: %#v %#v", detailsJob.Payload, detailsJob.Result)
	}
	if response := requestAs("/api/v1/jobs", "details-session"); response.Code != http.StatusForbidden {
		t.Fatalf("details-only list response=%d, want 403: %s", response.Code, response.Body.String())
	}

	readResponse := requestAs("/api/v1/jobs/job-a", "read-session")
	if readResponse.Code != http.StatusOK {
		t.Fatalf("read-only job response=%d %s", readResponse.Code, readResponse.Body.String())
	}
	readJob := decodeJob(readResponse)
	if readJob.Payload != nil || readJob.Result != nil {
		t.Fatalf("read-only job was not redacted: %#v %#v", readJob.Payload, readJob.Result)
	}

	if response := requestAs("/api/v1/jobs/job-a", "other-session"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-scope details response=%d, want 403: %s", response.Code, response.Body.String())
	}
}
