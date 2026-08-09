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
)

func TestBackupUploadChunkPollingOnlyRequiresRestorePermission(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "backup-upload-permissions.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}

	const (
		instanceID = "11111111-1111-4111-8111-111111111111"
		uploadID   = "22222222-2222-4222-8222-222222222222"
		jobID      = "33333333-3333-4333-8333-333333333333"
		wrongJobID = "44444444-4444-4444-8444-444444444444"
	)
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state)
			VALUES('restore-user','restore-user','restore-user','Restore user','hash','operator','active');
		INSERT INTO nodes(id,name,status) VALUES('restore-node','Restore node','online');
		INSERT INTO instances(id,node_id,name,status,port,config)
			VALUES(?1,'restore-node','Restore instance','stopped',27015,'{}');
		INSERT INTO role_bindings(id,user_id,role,scope_type,instance_id)
			VALUES('restore-binding','restore-user','operator','instance',?1);
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES
			('restore-binding','instances.read'),('restore-binding','backups.restore');
		INSERT INTO backup_uploads(id,instance_id,name,platform,size,sha256,received_size,status,requested_by,expires_at)
			VALUES(?2,?1,'portable.zip','linux/amd64',1024,?5,0,'uploading','restore-user',?6);
		INSERT INTO jobs(id,node_id,instance_id,type,status,progress,message,payload,result,requested_by) VALUES
			(?3,'restore-node',?1,'backup.upload.chunk','failed',100,'agent detail that must not be returned',
			 '{"upload_id":"22222222-2222-4222-8222-222222222222","content_base64_ciphertext":"sealed-secret"}',
			 '{"internal":"do-not-return"}','restore-user'),
			(?4,'restore-node',?1,'restore','failed',100,'wrong type',
			 '{"upload_id":"22222222-2222-4222-8222-222222222222"}','{}','restore-user');
	`, instanceID, uploadID, jobID, wrongJobID, strings.Repeat("a", 64), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	const sessionSecret = "restore-only-session"
	if _, err := store.CreateSession(ctx, "restore-user", tokenHash(sessionSecret), tokenHash("restore-csrf"), "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	principal, err := store.Session(ctx, tokenHash(sessionSecret))
	if err != nil {
		t.Fatal(err)
	}
	if principal.Allows(PermissionJobsRead) || principal.Allows(PermissionJobsDetails) {
		t.Fatal("restore-only principal unexpectedly has generic job permissions")
	}

	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	requestAsRestoreUser := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionSecret})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	response := requestAsRestoreUser("/api/v1/instances/" + instanceID + "/backup-uploads/" + uploadID + "/chunks/" + jobID)
	if response.Code != http.StatusOK {
		t.Fatalf("restore-only chunk poll returned %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["job_id"] != jobID || body["status"] != "failed" {
		t.Fatalf("unexpected fixed chunk response: %#v", body)
	}
	for _, secret := range []string{"message", "payload", "result", "sealed-secret", "do-not-return", "agent detail"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("chunk status response leaked %q: %s", secret, response.Body.String())
		}
	}
	if generic := requestAsRestoreUser("/api/v1/jobs/" + jobID); generic.Code != http.StatusForbidden {
		t.Fatalf("restore-only generic job read returned %d, want 403: %s", generic.Code, generic.Body.String())
	}
	if wrongType := requestAsRestoreUser("/api/v1/instances/" + instanceID + "/backup-uploads/" + uploadID + "/chunks/" + wrongJobID); wrongType.Code != http.StatusNotFound {
		t.Fatalf("wrong-type chunk poll returned %d, want 404: %s", wrongType.Code, wrongType.Body.String())
	}
}
