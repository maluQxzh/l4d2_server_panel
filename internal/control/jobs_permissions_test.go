package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

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
