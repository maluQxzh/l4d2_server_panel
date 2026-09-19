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

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func packageScanResult(names ...string) map[string]any {
	items := []any{}
	for _, name := range names {
		items = append(items, map[string]any{"name": name, "path": "left4dead2/addons/" + name, "sha256": strings.Repeat("a", 64), "size": 100, "vpk_version": 1})
	}
	return map[string]any{"kind": "installed_vpk_scan", "directory": "left4dead2/addons", "packages": items, "rejected": []any{}, "discovered_count": len(items), "valid_count": len(items), "rejected_count": 0}
}

func queuePackageScan(t *testing.T, s *Store) domain.Job {
	t.Helper()
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "scan_packages", map[string]any{"directory": "left4dead2/addons", "mode": "installed_vpk_v1"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func scanOutcome(t *testing.T, s *Store, id string) (string, int) {
	t.Helper()
	var outcome string
	var count int
	if err := s.db.QueryRow(`SELECT outcome,registered_count FROM package_scan_runs WHERE job_id=?1`, id).Scan(&outcome, &count); err != nil {
		t.Fatal(err)
	}
	return outcome, count
}

func projectScan(t *testing.T, s *Store, job domain.Job, result map[string]any) domain.Job {
	t.Helper()
	ctx := context.Background()
	if _, err := s.UpdateJob(ctx, job.ID, domain.JobSucceeded, 100, "scan complete", result); err != nil {
		t.Fatal(err)
	}
	projected, changed, err := s.ApplyPendingJobProjection(ctx, job.ID)
	if err != nil || !changed || jobProjectionPending(projected) {
		t.Fatalf("projection: %v %v", changed, err)
	}
	return projected
}

func TestParallelPackageScanWriteOverlapAndSlots(t *testing.T) {
	for _, writeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "write-first", false: "scan-first"}[writeFirst], func(t *testing.T) {
			s := packageRenameStore(t)
			ctx := context.Background()
			var scan, write domain.Job
			queueWrite := func() {
				var err error
				write, _, err = s.QueueInstanceJob(ctx, "instance", "stop", nil, "", "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if writeFirst {
				queueWrite()
			}
			scan = queuePackageScan(t, s)
			if !writeFirst {
				queueWrite()
			}
			var holdsWrite, holdsScan bool
			if err := s.db.QueryRow(`SELECT holds_instance_write_lock,holds_instance_scan_slot FROM jobs WHERE id=?1`, scan.ID).Scan(&holdsWrite, &holdsScan); err != nil || holdsWrite || !holdsScan {
				t.Fatal("wrong scan lock classification", err)
			}
			if _, _, err := s.QueueInstanceJob(ctx, "instance", "scan_packages", nil, "", ""); !errors.Is(err, ErrConflict) {
				t.Fatal("duplicate scan accepted", err)
			}
			if _, _, err := s.QueueInstanceJob(ctx, "instance", "stop", nil, "", ""); !errors.Is(err, ErrConflict) {
				t.Fatal("second write accepted", err)
			}
			if _, err := s.db.Exec(`UPDATE jobs SET status='succeeded',result='{}' WHERE id=?1`, write.ID); err != nil {
				t.Fatal(err)
			}
			projectScan(t, s, scan, packageScanResult("new.vpk"))
			if outcome, count := scanOutcome(t, s, scan.ID); outcome != "stale" || count != 0 {
				t.Fatalf("stale scan applied: %s %d", outcome, count)
			}
			fresh := queuePackageScan(t, s)
			projectScan(t, s, fresh, packageScanResult("new.vpk"))
			if outcome, count := scanOutcome(t, s, fresh.ID); outcome != "applied" || count != 1 {
				t.Fatalf("fresh scan not applied: %s %d", outcome, count)
			}
		})
	}
}

func TestParallelPackageScanCancellationAndProjectionSlot(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	scan := queuePackageScan(t, s)
	if _, err := s.MarkDispatched(ctx, scan.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CancelJob(ctx, scan.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "scan_packages", nil, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("cancel_pending released scan slot")
	}
	write, _, err := s.QueueInstanceJob(ctx, "instance", "stop", nil, "", "")
	if err != nil {
		t.Fatal("canceled scan blocks writes", err)
	}
	if _, err := s.UpdateJob(ctx, scan.ID, domain.JobCanceled, 0, "canceled", nil); err != nil {
		t.Fatal(err)
	}
	scan = queuePackageScan(t, s)
	if _, err := s.UpdateJob(ctx, scan.ID, domain.JobSucceeded, 100, "done", packageScanResult("new.vpk")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "scan_packages", nil, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("pending projection released scan slot")
	}
	if _, _, err := s.ApplyPendingJobProjection(ctx, scan.ID); err != nil {
		t.Fatal(err)
	}
	_ = queuePackageScan(t, s)
	var held bool
	if err := s.db.QueryRow(`SELECT holds_instance_write_lock FROM jobs WHERE id=?1`, write.ID).Scan(&held); err != nil || !held {
		t.Fatal("projection cleared another writer barrier")
	}
}

func TestParallelPackageScanIdempotencyNeverRefreshesSnapshot(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	payload := map[string]any{"directory": "left4dead2/addons", "mode": "installed_vpk_v1"}
	scan, _, err := s.QueueInstanceJob(ctx, "instance", "scan_packages", payload, "", "same-scan")
	if err != nil {
		t.Fatal(err)
	}
	write, _, err := s.QueueInstanceJob(ctx, "instance", "stop", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CancelJob(ctx, write.ID); err != nil {
		t.Fatal(err)
	}
	replay, reused, err := s.QueueInstanceJob(ctx, "instance", "scan_packages", payload, "", "same-scan")
	if err != nil || !reused || replay.ID != scan.ID {
		t.Fatal("scan replay lost", err)
	}
	projectScan(t, s, replay, packageScanResult("new.vpk"))
	if outcome, _ := scanOutcome(t, s, scan.ID); outcome != "stale" {
		t.Fatal("retry refreshed original snapshot")
	}
}

func TestParallelPackageScanInvalidatesAllInventoryChanges(t *testing.T) {
	for _, change := range []struct{ name, sql string }{
		{"delete", `DELETE FROM addon_packages WHERE id='package'`},
		{"rename", `UPDATE addon_packages SET path='left4dead2/addons/renamed.vpk' WHERE id='package'`},
		{"toggle", `UPDATE addon_packages SET enabled=0 WHERE id='package'`},
		{"rollback", `UPDATE addon_packages SET sha256='changed' WHERE id='package'`},
		{"manifest", `UPDATE addon_package_files SET size=101 WHERE package_id='package'`},
		{"manifest delete", `DELETE FROM addon_package_files WHERE package_id='package'`},
		{"instance target", `UPDATE instances SET path='/new-path' WHERE id='instance'`},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := packageRenameStore(t)
			ctx := context.Background()
			if _, err := s.db.Exec(`INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size) VALUES('package','instance','left4dead2/addons/old.vpk',?1,100)`, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			scan := queuePackageScan(t, s)
			if _, err := s.db.Exec(change.sql); err != nil {
				t.Fatal(err)
			}
			projectScan(t, s, scan, packageScanResult("old.vpk", "new.vpk"))
			if outcome, _ := scanOutcome(t, s, scan.ID); outcome != "stale" {
				t.Fatal("inventory change did not invalidate scan")
			}
			items, err := s.ListPackages(ctx, "instance")
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Path == "left4dead2/addons/new.vpk" {
					t.Fatal("partial stale inventory applied")
				}
			}
			if change.name == "delete" && len(items) != 0 {
				t.Fatal("deleted package resurrected")
			}
		})
	}
}

func TestParallelPackageScanReplayAfterDeleteAndRestart(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	scan := queuePackageScan(t, s)
	update := protocol.JobUpdate{Status: domain.JobSucceeded, Progress: 100, Message: "scan complete", Result: packageScanResult("new.vpk")}
	key := []byte("0123456789abcdef0123456789abcdef")
	fingerprint, err := terminalJobUpdateFingerprint(key, scan.ID, update)
	if err != nil {
		t.Fatal(err)
	}
	result := cloneMap(update.Result)
	result[terminalOutcomeFingerprintField] = fingerprint
	scan = projectScan(t, s, scan, result)
	if _, err := s.db.Exec(`DELETE FROM addon_packages WHERE path='left4dead2/addons/new.vpk'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenStore(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = reopened
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ApplyJobResult(ctx, scan); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	items, err := s.ListPackages(ctx, "instance")
	if err != nil || len(items) != 1 {
		t.Fatal("replay resurrected deleted package", err)
	}
	current, err := s.GetJob(ctx, scan.ID)
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := terminalReplayEquivalent(key, current, update, fingerprint)
	if err != nil || !equivalent {
		t.Fatal("projection changed immutable terminal outcome", err)
	}
	if outcome, count := scanOutcome(t, s, scan.ID); outcome != "applied" || count != 1 {
		t.Fatal("durable outcome changed on replay")
	}
}

func TestParallelPackageScanReadsDoNotInvalidateAndOwnedFilesNotAdopted(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size) VALUES('package','instance','left4dead2/addons/owned.vpk',?1,100)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	scan := queuePackageScan(t, s)
	for _, action := range []string{"a2s.info", "inspect_plugin_archive", "prepare_package"} {
		job, _, err := s.QueueInstanceJob(ctx, "instance", action, nil, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateJob(ctx, job.ID, domain.JobFailed, 100, "read failed", nil); err != nil {
			t.Fatal(err)
		}
	}
	projectScan(t, s, scan, packageScanResult("owned.vpk", "new.vpk"))
	if outcome, count := scanOutcome(t, s, scan.ID); outcome != "applied" || count != 1 {
		t.Fatalf("readonly work or ownership mishandled: %s %d", outcome, count)
	}
	items, err := s.ListPackages(ctx, "instance")
	if err != nil || len(items) != 2 {
		t.Fatal("owned file adopted separately", err)
	}
}

func TestParallelPackageScanDeletedInstanceStillSettlesTerminal(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	scan := queuePackageScan(t, s)
	if err := s.DeleteInstanceAfterJob(ctx, "instance"); err != nil {
		t.Fatal(err)
	}
	projected := projectScan(t, s, scan, packageScanResult("new.vpk"))
	if projected.InstanceID != "" {
		t.Fatal("deleted instance scope was recreated")
	}
	if _, err := s.GetInstance(ctx, "instance"); !errors.Is(err, ErrNotFound) {
		t.Fatal("instance resurrected", err)
	}
}

func TestParallelPackageScanStatusPermissionAndNoPayload(t *testing.T) {
	s := packageRenameStore(t)
	server := recoveryServer(t, s)
	scan := queuePackageScan(t, s)
	if _, _, err := s.QueueInstanceJob(context.Background(), "instance", "stop", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	projectScan(t, s, scan, packageScanResult("secret-name.vpk"))
	for _, role := range []domain.Role{domain.RoleViewer, domain.RoleOwner} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", "instance")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		req = req.WithContext(context.WithValue(req.Context(), principalContextKey, Principal{User: domain.User{Role: role}}))
		res := httptest.NewRecorder()
		server.requireInstancePermission(PermissionPackagesInstall)(http.HandlerFunc(server.handleInstalledPackageScanStatus)).ServeHTTP(res, req)
		if role == domain.RoleViewer {
			if res.Code != http.StatusForbidden {
				t.Fatal("unprivileged scan status exposed")
			}
			continue
		}
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"outcome":"stale"`) || strings.Contains(res.Body.String(), "secret-name") {
			t.Fatal("invalid scan status", res.Body.String())
		}
	}
}

func TestParallelPackageScanMigrationRejectsLegacyProjection(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join("..", "..", "migrations", "sqlite")
	old := t.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "022_" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(old, entry.Name()), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenStore(ctx, filepath.Join(t.TempDir(), "legacy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx, old); err != nil {
		t.Fatal(err)
	}
	result := packageScanResult("old.vpk")
	result[jobProjectionPendingField] = true
	raw, _ := json.Marshal(result)
	if _, err := s.db.Exec(`INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('instance','node','Server','stopped','/game',27015,'{}');
		INSERT INTO jobs(id,node_id,instance_id,type,status,result,idempotency_key,request_fingerprint) VALUES('legacy-scan','node','instance','scan_packages','succeeded',?1,'scan-key',?2)`, string(raw), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx, root); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.ApplyPendingJobProjection(ctx, "legacy-scan")
	if err != nil || jobProjectionPending(job) || job.IdempotencyKey != "scan-key" {
		t.Fatal("migration lost legacy job", err)
	}
	if outcome, _ := scanOutcome(t, s, job.ID); outcome != "stale" {
		t.Fatal("legacy scan trusted without a snapshot")
	}
	items, err := s.ListPackages(ctx, "instance")
	if err != nil || len(items) != 0 {
		t.Fatal("legacy inventory applied", err)
	}
}
