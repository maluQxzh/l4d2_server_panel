package control

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func openNodeTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

func TestEnrollmentTokenLabelBecomesNodeName(t *testing.T) {
	store, ctx := openNodeTestStore(t)
	token, _, err := store.CreateEnrollmentToken(ctx, "  香港-02  ", "", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := store.ConsumeEnrollmentToken(ctx, EnrollRequest{
		Token: token, Name: "fixed-hostname", OS: "linux", Arch: "amd64", InstanceRoot: "/srv/l4d2",
	}, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if node.Name != "香港-02" {
		t.Fatalf("node name=%q, want enrollment name", node.Name)
	}

	fallbackToken, _, err := store.CreateEnrollmentToken(ctx, "", "", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fallback, _, err := store.ConsumeEnrollmentToken(ctx, EnrollRequest{
		Token: fallbackToken, Name: "legacy-agent-name", OS: "linux", Arch: "amd64", InstanceRoot: "/srv/legacy",
	}, "192.0.2.11")
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Name != "legacy-agent-name" {
		t.Fatalf("fallback node name=%q", fallback.Name)
	}
	if _, _, err := store.CreateEnrollmentToken(ctx, strings.Repeat("n", 129), "", 30*time.Minute); err == nil {
		t.Fatal("overlong enrollment node name was accepted")
	}
}

func TestDeleteNodeRemovesControlPlaneRelationsAndRevokesCredential(t *testing.T) {
	store, ctx := openNodeTestStore(t)
	token, _, err := store.CreateEnrollmentToken(ctx, "delete-me", "", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	node, credential, err := store.ConsumeEnrollmentToken(ctx, EnrollRequest{
		Token: token, Name: "ignored-hostname", OS: "linux", Arch: "amd64", InstanceRoot: "/srv/l4d2",
	}, "192.0.2.12")
	if err != nil {
		t.Fatal(err)
	}
	userID, instanceID, installationID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	jobID, mapID, auditID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role)
			VALUES(?1,'node-owner','node-owner','Node Owner','hash','owner');
		INSERT INTO game_installations(id,environment_id,node_id,path,path_key,status)
			VALUES(?2,'environment-' || ?3,?3,'/srv/l4d2/game','/srv/l4d2/game','ready');
		INSERT INTO instances(id,node_id,installation_id,name,status,port)
			VALUES(?4,?3,?2,'instance','stopped',27015);
		INSERT INTO jobs(id,node_id,instance_id,type,status,requested_by)
			VALUES(?5,?3,?4,'start','failed',?1);
		INSERT INTO maps(id,node_id,name,aggregate_sha256,total_size,created_by)
			VALUES(?6,?3,'Map',?7,1,?1);
		INSERT INTO map_assignments(map_id,instance_id) VALUES(?6,?4);
		INSERT INTO metrics(node_id,instance_id,collected_at) VALUES(?3,?4,?8);
		INSERT INTO role_bindings(id,user_id,role,scope_type,node_id,created_by)
			VALUES(?9,?1,'admin','node',?3,?1);
		INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask,node_id,expires_at)
			VALUES(?10,?1,zeroblob(32),'node token',7,?3,?11);
		INSERT INTO notifications(id,severity,type,title,message,resource,metadata)
			VALUES(?12,'warning','job.failed','failed','failed',?5,json_object('node_id',?3,'instance_id',?4,'job_id',?5));
		INSERT INTO audit_events(id,actor_name,action,resource_type,resource_id)
			VALUES(?13,'owner','node.created','node',?3);
		INSERT INTO quarantine_entries(id,node_id,source_job_id,original_instance_id,instance_name,root_index,entry_name,quarantined_at,retention_expires_at)
			VALUES(?14,?3,?5,?4,'instance',0,'instance-20260809T000000Z',?15,?16);
	`, userID, installationID, node.ID, instanceID, jobID, mapID, strings.Repeat("a", 64), now,
		uuid.NewString(), uuid.NewString(), now.Add(time.Hour), uuid.NewString(), auditID, uuid.NewString(), now.UnixMicro(), now.Add(7*24*time.Hour).UnixMicro()); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	for table := range map[string]struct{}{
		"nodes": {}, "instances": {}, "jobs": {}, "maps": {}, "metrics": {}, "environments": {},
		"game_installations": {}, "role_bindings": {}, "api_tokens": {}, "notifications": {}, "quarantine_entries": {},
	} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s retained %d node-related rows", table, count)
		}
	}
	var auditCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE id=?1`, auditID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit history count=%d err=%v", auditCount, err)
	}
	if _, err := store.NodeByCredential(ctx, credential); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleted node credential error=%v", err)
	}
	if err := store.DeleteNode(ctx, node.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete error=%v", err)
	}
}
