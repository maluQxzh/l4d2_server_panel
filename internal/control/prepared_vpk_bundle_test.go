package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func readyPreparedBundle(t *testing.T, s *Store) (domain.Job, domain.PreparedVPKBundle) {
	t.Helper()
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "prepare_package", map[string]any{"kind": domain.PreparedVPKBundleKind, "name": "", "source_transport": "qq_flash"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := domain.PreparedVPKBundle{PreparedPackage: domain.PreparedPackage{ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Kind: domain.PreparedVPKBundleKind, Name: domain.PreparedVPKBundleName, ExpiresAt: time.Now().UTC().Add(time.Hour), Size: 200}, Files: []domain.PreparedVPKFile{{Name: "one.vpk", SHA256: strings.Repeat("a", 64), Size: 100, Version: 2}, {Name: "two.vpk", SHA256: strings.Repeat("b", 64), Size: 100, Version: 1}}}
	bundle.SHA256 = bundle.Digest()
	raw, _ := json.Marshal(bundle)
	_ = json.Unmarshal(raw, &job.Result)
	job, err = s.UpdateJob(context.Background(), job.ID, domain.JobSucceeded, 100, "ready", job.Result)
	if err != nil {
		t.Fatal(err)
	}
	return job, bundle
}

func bundleProjection(t *testing.T, job domain.Job, bundle domain.PreparedVPKBundle) domain.Job {
	t.Helper()
	job.Status = domain.JobSucceeded
	files := []any{}
	for index, file := range bundle.Files {
		target, err := domain.PreparedVPKBundleTarget(bundle.ID, index, file.Name)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, map[string]any{"name": file.Name, "kind": "vpk", "path": target, "sha256": file.SHA256, "size": file.Size, "vpk_version": file.Version})
	}
	job.Result = map[string]any{"kind": "qq_flash_vpk", "packages": files, "files_imported": len(files)}
	return job
}

func TestPreparedVPKBundleQueueProjectionAndOwnership(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	p := installQueuePrincipal(t, s)
	prepared, bundle := readyPreparedBundle(t, s)
	queue := submitInstallQueue(t, s, p, prepared.ID)
	if changed, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, false); err != nil || changed {
		t.Fatalf("old Agent created bundle writer: %v %v", changed, err)
	}
	if child := queueChild(t, s, queue.ID, 0); child.ID != "" {
		t.Fatal("incompatible Agent holds write barrier")
	}
	if _, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true); err != nil {
		t.Fatal(err)
	}
	child := queueChild(t, s, queue.ID, 0)
	if child.Payload["kind"] != domain.PreparedVPKBundleKind || child.Payload["expected_sha256"] != bundle.SHA256 {
		t.Fatal("bundle metadata lost")
	}
	projected := bundleProjection(t, child, bundle)
	if err := s.ApplyJobResult(ctx, projected); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyJobResult(ctx, projected); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM addon_packages WHERE instance_id='instance' AND source='qq_flash'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("projection count=%d err=%v", count, err)
	}
}

func TestPreparedVPKBundleRejectsForgedProjectionAndMetadata(t *testing.T) {
	for _, scenario := range []string{"path", "hash", "count", "size", "fractional-size", "manifest"} {
		t.Run(scenario, func(t *testing.T) {
			s := packageRenameStore(t)
			ctx := context.Background()
			prepared, bundle := readyPreparedBundle(t, s)
			child, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": prepared.ID}, "", "")
			if err != nil {
				t.Fatal(err)
			}
			projected := bundleProjection(t, child, bundle)
			files := projected.Result["packages"].([]any)
			switch scenario {
			case "path":
				files[0].(map[string]any)["path"] = "left4dead2/addons/other.vpk"
			case "hash":
				files[0].(map[string]any)["sha256"] = strings.Repeat("c", 64)
			case "size":
				files[0].(map[string]any)["size"] = 200
			case "fractional-size":
				files[0].(map[string]any)["size"] = 100.5
			case "count":
				projected.Result["files_imported"] = 3
			case "manifest":
				if _, err := s.db.Exec(`UPDATE jobs SET result=json_set(result,'$.files[0].name','forged.vpk') WHERE id=?1`, prepared.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ApplyJobResult(ctx, projected); err == nil {
				t.Fatal("forged bundle projection accepted")
			}
			var count int
			_ = s.db.QueryRow(`SELECT count(*) FROM addon_packages WHERE source='qq_flash'`).Scan(&count)
			if count != 0 {
				t.Fatal("partially projected invalid bundle")
			}
		})
	}
}

func TestPreparedVPKBundleQuotaAndOwnedTarget(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	prepared, bundle := readyPreparedBundle(t, s)
	target, _ := domain.PreparedVPKBundleTarget(prepared.ID, 0, bundle.Files[0].Name)
	if _, err := s.db.Exec(`INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk) VALUES('owner','instance','owned','vpk','v1','upload','','a',?1,true,'normal')`, target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": prepared.ID}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("owned path allowed: %v", err)
	}
	for i := 1; i < domain.PreparedVPKBundleSlots; i++ {
		readyPreparedBundle(t, s)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", map[string]any{"kind": domain.PreparedVPKBundleKind}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("bundle quota bypassed: %v", err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", map[string]any{"kind": "vpk", "name": "single.vpk"}, "", ""); err != nil {
		t.Fatalf("bundle quota blocked single file: %v", err)
	}
}
