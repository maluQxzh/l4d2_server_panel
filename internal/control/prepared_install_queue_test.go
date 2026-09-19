package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func installQueuePrincipal(t *testing.T, s *Store) Principal {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,account_state) VALUES('installer','installer','installer','Installer','unused','operator','active');
		INSERT INTO role_bindings(id,user_id,role,scope_type,instance_id) VALUES('install-binding','installer','operator','instance','instance');
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES('install-binding','packages.install')`)
	if err != nil {
		t.Fatal(err)
	}
	return Principal{User: domain.User{ID: "installer"}}
}

func submitInstallQueue(t *testing.T, s *Store, p Principal, ids ...string) domain.Job {
	t.Helper()
	request := preparedInstallQueueRequest{IDs: ids}
	job, _, err := s.QueuePreparedInstalls(context.Background(), "instance", request, p, "batch", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func queueChild(t *testing.T, s *Store, id string, position int) domain.Job {
	t.Helper()
	var childID string
	if err := s.db.QueryRow(`SELECT COALESCE(install_job_id,'') FROM prepared_install_queue_items WHERE queue_id=?1 AND position=?2`, id, position).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	if childID == "" {
		return domain.Job{}
	}
	job, err := s.GetJob(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPreparedInstallQueueSerialRestartAndBarriers(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	p := installQueuePrincipal(t, s)
	first, second := readyPreparedJob(t, s), readyPreparedJob(t, s)
	blocker, _, err := s.QueueInstanceJob(ctx, "instance", "stop", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	batch := submitInstallQueue(t, s, p, first.ID, second.ID)
	if changed, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil || changed {
		t.Fatalf("busy advanced: %v %v", changed, err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "discard_prepared_package", map[string]any{"prepared_package_id": second.ID}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reservation ignored: %v", err)
	}
	jobs, err := s.JobsForDispatch(ctx, "node", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.ID == batch.ID {
			t.Fatal("coordinator sent to Agent")
		}
	}
	if err := validateJobUpdate(batch, "node", protocol.Envelope{JobID: batch.ID}, protocol.JobUpdate{Status: domain.JobSucceeded}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("Agent can complete coordinator")
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status='succeeded',result='{"_projection_pending":true}' WHERE id=?1`, blocker.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil || changed {
		t.Fatal("projection barrier bypassed")
	}
	if _, err := s.db.Exec(`UPDATE jobs SET result='{}' WHERE id=?1`, blocker.ID); err != nil {
		t.Fatal(err)
	}
	// Recover through a fresh Store, without any browser state or in-memory loop.
	s.Close()
	reopened, err := OpenStore(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	s = reopened
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	child := queueChild(t, s, batch.ID, 0)
	if child.ID == "" || queueChild(t, s, batch.ID, 1).ID != "" {
		t.Fatal("not one child at a time")
	}
	if claimed, err := s.MarkDispatched(ctx, child.ID); err != nil || !claimed {
		t.Fatalf("dispatch %v %v", claimed, err)
	}
	if _, _, err := s.CancelJob(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil || changed {
		t.Fatal("pending cancellation barrier bypassed")
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status='succeeded',result='{"_projection_pending":true}' WHERE id=?1`, child.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil || changed {
		t.Fatal("child projection barrier bypassed")
	}
	if _, err := s.db.Exec(`UPDATE jobs SET result='{}' WHERE id=?1`, child.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.advancePreparedInstallQueue(ctx, batch.ID, false); err != nil || changed {
		t.Fatal("incompatible Agent got child")
	}
	if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	child2 := queueChild(t, s, batch.ID, 1)
	if child2.ID == "" || child2.ID == child.ID {
		t.Fatal("second child missing")
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status='succeeded',result='{}' WHERE id=?1`, child2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	finished, _ := s.GetJob(ctx, batch.ID)
	if finished.Status != domain.JobSucceeded {
		t.Fatalf("not finished: %+v", finished)
	}
	replay, reused, err := s.QueuePreparedInstalls(ctx, "instance", preparedInstallQueueRequest{IDs: []string{first.ID, second.ID}}, p, "batch", strings.Repeat("b", 64))
	if err != nil || !reused || replay.ID != batch.ID {
		t.Fatalf("lost replay: %v", err)
	}
	if _, _, err := s.QueuePreparedInstalls(ctx, "instance", preparedInstallQueueRequest{IDs: []string{first.ID}}, p, "batch", strings.Repeat("c", 64)); !errors.Is(err, ErrConflict) {
		t.Fatal("changed body reused")
	}
}

func TestPreparedInstallQueueReauthorizationAndExpiry(t *testing.T) {
	for _, change := range []struct {
		name, sql string
		token     bool
	}{
		{"permission removed", `DELETE FROM role_binding_permissions`, false},
		{"user disabled", `UPDATE users SET disabled=1`, false},
		{"activation reset", `UPDATE users SET account_state='pending_activation'`, false},
		{"user deleted", `DELETE FROM users`, false},
		{"token revoked", `UPDATE api_tokens SET revoked_at=1`, true},
		{"token deleted", `DELETE FROM api_tokens`, true},
		{"token narrowed", `UPDATE api_tokens SET scope_mask=1`, true},
		{"token expired", `UPDATE api_tokens SET expires_at=1`, true},
		{"prepared expired", `UPDATE jobs SET result=json_set(result,'$.expires_at','2020-01-01T00:00:00Z') WHERE type='prepare_package'`, false},
		{"external instance", `UPDATE instances SET status='external'`, false},
	} {
		for _, beforeDispatch := range []bool{false, true} {
			t.Run(change.name+map[bool]string{true: " before dispatch", false: " waiting"}[beforeDispatch], func(t *testing.T) {
				s := packageRenameStore(t)
				p := installQueuePrincipal(t, s)
				ctx := context.Background()
				if change.token {
					_, err := s.db.Exec(`INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask,instance_id,expires_at) VALUES('token','installer',zeroblob(32),'queue',4,'instance',?1)`, time.Now().Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					p.APITokenID = "token"
				}
				prepared := readyPreparedJob(t, s)
				batch := submitInstallQueue(t, s, p, prepared.ID)
				if beforeDispatch {
					if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.db.Exec(change.sql); err != nil {
					t.Fatal(err)
				}
				if beforeDispatch {
					child := queueChild(t, s, batch.ID, 0)
					if claimed, err := s.MarkDispatched(ctx, child.ID); err != nil || claimed {
						t.Fatalf("dispatch after invalidation: %v %v", claimed, err)
					}
				}
				if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
					t.Fatal(err)
				}
				finished, _ := s.GetJob(ctx, batch.ID)
				if finished.Status != domain.JobFailed {
					t.Fatalf("not failed: %+v", finished)
				}
				if !beforeDispatch && queueChild(t, s, batch.ID, 0).ID != "" {
					t.Fatal("invalid intent created a child")
				}
			})
		}
	}
}

func TestPreparedInstallQueueStopAndFailureReleaseWaiting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "failed", false: "stopped"}[fail], func(t *testing.T) {
			s := packageRenameStore(t)
			p := installQueuePrincipal(t, s)
			ctx := context.Background()
			one, two := readyPreparedJob(t, s), readyPreparedJob(t, s)
			batch := submitInstallQueue(t, s, p, one.ID, two.ID)
			if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
				t.Fatal(err)
			}
			child := queueChild(t, s, batch.ID, 0)
			if fail {
				if _, err := s.UpdateJob(ctx, child.ID, domain.JobFailed, 100, "failed", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
					t.Fatal(err)
				}
			} else {
				job, _, err := s.CancelJob(ctx, batch.ID)
				if err != nil || job.Result["cancel_pending"] != nil {
					t.Fatalf("stop: %v", err)
				}
				if _, _, err := s.CancelJob(ctx, batch.ID); err != nil {
					t.Fatal("stop not idempotent", err)
				}
				current, _ := s.GetJob(ctx, child.ID)
				if current.Status != domain.JobQueued {
					t.Fatal("stop canceled submitted child")
				}
			}
			if _, _, err := s.QueueInstanceJob(ctx, "instance", "discard_prepared_package", map[string]any{"prepared_package_id": two.ID}, "", ""); err != nil {
				t.Fatal("waiting reservation not released", err)
			}
			if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
				t.Fatal(err)
			}
			if queueChild(t, s, batch.ID, 1).ID != "" {
				t.Fatal("stopped queue advanced")
			}
		})
	}
}

func TestPreparedInstallQueueHTTPAndAuthorization(t *testing.T) {
	s := packageRenameStore(t)
	p := installQueuePrincipal(t, s)
	prepared := readyPreparedJob(t, s)
	server := recoveryServer(t, s)
	request := preparedInstallQueueRequest{IDs: []string{prepared.ID}}
	body, _ := json.Marshal(request)
	call := func(principal Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.Header.Set("Idempotency-Key", "http-batch")
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", "instance")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		req = req.WithContext(context.WithValue(req.Context(), principalContextKey, principal))
		res := httptest.NewRecorder()
		server.handleQueuePreparedInstalls(res, req)
		return res
	}
	if res := call(Principal{User: domain.User{ID: "viewer", Role: domain.RoleOwner}}); res.Code != http.StatusUnauthorized {
		t.Fatalf("store trusted caller role: %d %s", res.Code, res.Body.String())
	}
	res := call(p)
	if res.Code != http.StatusAccepted {
		t.Fatalf("accept: %d %s", res.Code, res.Body.String())
	}
	if res := call(p); res.Code != http.StatusAccepted || !strings.Contains(res.Body.String(), `"idempotent_replay":true`) {
		t.Fatal("HTTP replay failed", res.Body.String())
	}
}

func TestPreparedInstallQueueAtomicValidationAndOwnership(t *testing.T) {
	s := packageRenameStore(t)
	p := installQueuePrincipal(t, s)
	ctx := context.Background()
	first, second := readyPreparedJob(t, s), readyPreparedJob(t, s)
	for _, ids := range [][]string{{first.ID, first.ID}, {first.ID, "00000000-0000-0000-0000-000000000001"}, {}} {
		if _, _, err := s.QueuePreparedInstalls(ctx, "instance", preparedInstallQueueRequest{IDs: ids}, p, "invalid", strings.Repeat("a", 64)); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs WHERE type='prepared_install_queue'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial batch persisted", err)
	}
	batch := submitInstallQueue(t, s, p, first.ID)
	if _, _, err := s.QueuePreparedInstalls(ctx, "instance", preparedInstallQueueRequest{IDs: []string{second.ID}}, p, "another", strings.Repeat("c", 64)); !errors.Is(err, ErrConflict) {
		t.Fatal("two active batches accepted", err)
	}
	if _, err := s.db.Exec(`INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size) VALUES('package','instance','left4dead2/addons/ready.vpk',?1,100)`, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetJob(ctx, batch.ID)
	if current.Status != domain.JobFailed || queueChild(t, s, batch.ID, 0).ID != "" {
		t.Fatal("changed ownership ignored")
	}
	// A token scoped to another instance cannot borrow its user's binding.
	if _, err := s.db.Exec(`INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('other','node','Other','stopped','/other',27025,'{}');
		INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask,instance_id,expires_at) VALUES('scoped','installer',zeroblob(32),'queue',4,'other',?1)`, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p.APITokenID = "scoped"
	if _, _, err := s.QueuePreparedInstalls(ctx, "instance", preparedInstallQueueRequest{IDs: []string{second.ID}}, p, "scoped", strings.Repeat("e", 64)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("token scope ignored", err)
	}
}

func TestPreparedInstallQueueExpiresUnsentChildWhileOffline(t *testing.T) {
	s := packageRenameStore(t)
	p := installQueuePrincipal(t, s)
	ctx := context.Background()
	prepared := readyPreparedJob(t, s)
	batch := submitInstallQueue(t, s, p, prepared.ID)
	if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	child := queueChild(t, s, batch.ID, 0)
	if _, err := s.db.Exec(`UPDATE jobs SET result=json_set(result,'$.expires_at','2020-01-01T00:00:00Z') WHERE id=?1`, prepared.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advancePreparedInstallQueue(ctx, batch.ID, false); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetJob(ctx, child.ID)
	if err != nil || current.Status != domain.JobFailed {
		t.Fatal("unsent expired child kept the barrier", err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "stop", nil, "", ""); err != nil {
		t.Fatal("expired child still blocks writes", err)
	}
}

func TestPreparedInstallQueueMigrationPreservesExistingJobs(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join("..", "..", "migrations", "sqlite")
	oldRoot := t.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "021_" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldRoot, entry.Name()), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "upgrade.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx, oldRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','/game',27015,'{}');
		INSERT INTO jobs(id,node_id,instance_id,type,status,payload,result,idempotency_key,request_fingerprint) VALUES('old','node','instance','install_mod','succeeded','{"prepared_package_id":"prepared"}','{"_projection_pending":true}','old-key',?1)`, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx, root); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetJob(ctx, "old")
	if err != nil || job.IdempotencyKey != "old-key" || !jobProjectionPending(job) {
		t.Fatal("old job changed", err)
	}
	var locked bool
	if err := s.db.QueryRow(`SELECT holds_instance_write_lock FROM jobs WHERE id='old'`).Scan(&locked); err != nil || !locked {
		t.Fatal("old projection barrier lost", err)
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign keys broken")
	}
}
