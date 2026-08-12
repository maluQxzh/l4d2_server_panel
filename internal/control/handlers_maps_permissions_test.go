package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstanceScopedSharedMapPermissionsEnforceDeleteOwnership(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "map-permissions.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}

	const (
		userID      = "11111111-1111-4111-8111-111111111111"
		otherUserID = "22222222-2222-4222-8222-222222222222"
		nodeID      = "33333333-3333-4333-8333-333333333333"
		instanceID  = "44444444-4444-4444-8444-444444444444"
		bindingID   = "55555555-5555-4555-8555-555555555555"
		ownMapID    = "66666666-6666-4666-8666-666666666666"
		otherMapID  = "77777777-7777-4777-8777-777777777777"
	)
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state) VALUES
			(?1,'map-user','map-user','Map user','hash','admin','active'),
			(?2,'other-user','other-user','Other user','hash','admin','active');
		INSERT INTO nodes(id,name,status) VALUES(?3,'Shared map node','online');
		INSERT INTO instances(id,node_id,name,status,port,config) VALUES(?4,?3,'Scoped instance','stopped',27015,'{}');
		INSERT INTO role_bindings(id,user_id,role,scope_type,instance_id) VALUES(?5,?1,'admin','instance',?4);
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES
			(?5,'instances.read'),(?5,'maps.read'),(?5,'maps.import'),(?5,'maps.assign'),(?5,'maps.verify'),(?5,'maps.delete'),
			(?5,'jobs.read'),(?5,'jobs.details'),(?5,'jobs.cancel');
		INSERT INTO maps(id,node_id,name,aggregate_sha256,total_size,created_by) VALUES
			(?6,?3,'Own map',?8,1,?1),
			(?7,?3,'Other map',?9,1,?2);
	`, userID, otherUserID, nodeID, instanceID, bindingID, ownMapID, otherMapID, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}

	const sessionSecret = "instance-map-session"
	const csrfSecret = "instance-map-csrf"
	if _, err := store.CreateSession(ctx, userID, tokenHash(sessionSecret), tokenHash(csrfSecret), "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var payload *bytes.Reader
		if body == "" {
			payload = bytes.NewReader(nil)
		} else {
			payload = bytes.NewReader([]byte(body))
		}
		req := httptest.NewRequest(method, path, payload)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionSecret})
		req.Header.Set("X-CSRF-Token", csrfSecret)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	cancelActiveMapJob := func(jobType string) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='canceled',finished_at=?2 WHERE type=?1 AND status='queued'`, jobType, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	assertScopedJob := func(response *httptest.ResponseRecorder, wantType string) string {
		t.Helper()
		var reference struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &reference); err != nil || reference.JobID == "" {
			t.Fatalf("decode %s job reference: id=%q err=%v", wantType, reference.JobID, err)
		}
		job, err := store.GetJob(ctx, reference.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Type != wantType || job.ScopeInstanceID != instanceID {
			t.Fatalf("queued map job scope=%#v, want type=%s scope=%s", job, wantType, instanceID)
		}
		return reference.JobID
	}

	listResponse := request(http.MethodGet, "/api/v1/maps", "")
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "Own map") || !strings.Contains(listResponse.Body.String(), "Other map") {
		t.Fatalf("instance-scoped map list=%d %s", listResponse.Code, listResponse.Body.String())
	}

	assignResponse := request(http.MethodPut, "/api/v1/maps/"+otherMapID+"/assignments/"+instanceID, "")
	if assignResponse.Code != http.StatusAccepted {
		t.Fatalf("instance-scoped map assignment=%d %s", assignResponse.Code, assignResponse.Body.String())
	}
	cancelActiveMapJob("map.assign")

	importResponse := request(http.MethodPost, "/api/v1/nodes/"+nodeID+"/maps/imports", `{"share_url":"https://qfile.qq.com/q/Example123"}`)
	if importResponse.Code != http.StatusAccepted {
		t.Fatalf("instance-scoped map import=%d %s", importResponse.Code, importResponse.Body.String())
	}
	importJobID := assertScopedJob(importResponse, "map.import.qq_flash")
	secondImportResponse := request(http.MethodPost, "/api/v1/nodes/"+nodeID+"/maps/imports", `{"share_url":"https://qfile.qq.com/q/Another456"}`)
	if secondImportResponse.Code != http.StatusConflict {
		t.Fatalf("concurrent map import=%d, want 409 instead of reusing another task: %s", secondImportResponse.Code, secondImportResponse.Body.String())
	}
	cancelActiveMapJob("map.import.qq_flash")

	verifyResponse := request(http.MethodPost, "/api/v1/maps/"+otherMapID+"/verify", "")
	if verifyResponse.Code != http.StatusAccepted {
		t.Fatalf("instance-scoped map verification=%d %s", verifyResponse.Code, verifyResponse.Body.String())
	}
	assertScopedJob(verifyResponse, "map.verify")
	cancelActiveMapJob("map.verify")

	otherDeleteResponse := request(http.MethodPost, "/api/v1/maps/"+otherMapID+"/delete-confirmations", "")
	if otherDeleteResponse.Code != http.StatusForbidden {
		t.Fatalf("other user's map delete confirmation=%d, want 403: %s", otherDeleteResponse.Code, otherDeleteResponse.Body.String())
	}

	ownDeleteConfirmation := request(http.MethodPost, "/api/v1/maps/"+ownMapID+"/delete-confirmations", "")
	if ownDeleteConfirmation.Code != http.StatusCreated {
		t.Fatalf("own map delete confirmation=%d %s", ownDeleteConfirmation.Code, ownDeleteConfirmation.Body.String())
	}
	var confirmation struct {
		Token string `json:"confirmation_token"`
	}
	if err := json.Unmarshal(ownDeleteConfirmation.Body.Bytes(), &confirmation); err != nil || confirmation.Token == "" {
		t.Fatalf("decode own map confirmation: token=%q err=%v", confirmation.Token, err)
	}
	deleteResponse := request(http.MethodDelete, "/api/v1/maps/"+ownMapID, `{"confirmation_token":"`+confirmation.Token+`"}`)
	if deleteResponse.Code != http.StatusAccepted {
		t.Fatalf("own map deletion=%d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	assertScopedJob(deleteResponse, "map.delete")

	jobsResponse := request(http.MethodGet, "/api/v1/jobs?background=exclude", "")
	if jobsResponse.Code != http.StatusOK || !strings.Contains(jobsResponse.Body.String(), importJobID) ||
		!strings.Contains(jobsResponse.Body.String(), `"scope_instance_id":"`+instanceID+`"`) {
		t.Fatalf("instance-scoped task center=%d %s", jobsResponse.Code, jobsResponse.Body.String())
	}
}
