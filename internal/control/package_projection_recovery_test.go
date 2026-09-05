package control

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestVPKInstallProjectionRecoveryReleasesInstanceLock(t *testing.T) {
	ctx := context.Background()
	store, instance := projectionRecoveryStore(t)
	owner, _, err := store.BootstrapOwner(ctx, "incident-owner", "Owner", "hash")
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.QueueInstanceJob(ctx, instance.ID, "install_mod", map[string]any{
		"kind": "vpk_url", "name": "【基础】看脚插件.vpk", "risk_confirmed": false,
		"acknowledge_live_changes": false, "acknowledge_native_risk": false, "acknowledge_restart_required": false,
	}, owner.ID, "incident-probe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatched(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.UpdateJob(ctx, job.ID, domain.JobSucceeded, 100, "operation completed", map[string]any{
		"kind": "vpk", "path": "left4dead2/addons/【基础】看脚插件.vpk",
		"sha256": "7c66345377eeee8958938002d0d714aa54583d06d2ac316c796c11cb932da84d", "size": 196870, "vpk_version": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := recoveryServer(t, store)
	s.reconcilePendingJobProjections(ctx)
	settled, err := store.GetJob(ctx, job.ID)
	if err != nil || jobProjectionPending(settled) {
		t.Fatalf("projection not settled: %v, %#v", err, settled)
	}
	packages, err := store.ListPackages(ctx, instance.ID)
	if err != nil || len(packages) != 1 || packages[0].SHA256 != "7c66345377eeee8958938002d0d714aa54583d06d2ac316c796c11cb932da84d" {
		t.Fatalf("packages = %#v, err=%v", packages, err)
	}
	if _, _, err := store.QueueInstanceJob(ctx, instance.ID, "delete", map[string]any{"mode": "detach"}, owner.ID, "incident-detach"); err != nil {
		t.Fatal(err)
	}
}

func TestPruneStoredPackageVersionsKeepsNewestThree(t *testing.T) {
	for _, count := range []int{0, 3, 5} {
		t.Run(fmt.Sprintf("versions_%d", count), func(t *testing.T) {
			ctx := context.Background()
			store, instance := projectionRecoveryStore(t)
			if _, err := store.db.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind) VALUES
				('target',?1,'target.vpk','vpk'),('other',?1,'other.vpk','vpk')`, instance.ID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < count; i++ {
				// Equal timestamps exercise the deterministic ID tie-breaker.
				if _, err := store.db.ExecContext(ctx, `INSERT INTO addon_package_versions
					(id,package_id,instance_id,source_job_id,version,sha256,storage_path,target_path,created_at)
					VALUES(?1,'target',?2,?1,?1,?3,?1,'left4dead2/addons/target.vpk',?4)`,
					fmt.Sprintf("version-%d", i), instance.ID, strings.Repeat("a", 64), time.Unix(1000, 0).UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.ExecContext(ctx, `INSERT INTO addon_package_versions
				(id,package_id,instance_id,source_job_id,version,sha256,storage_path,target_path)
				VALUES('other-version','other',?1,'other-job','old',?2,'other.bin','left4dead2/addons/other.vpk')`, instance.ID, strings.Repeat("b", 64)); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				tx, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := pruneStoredPackageVersions(ctx, tx, "target"); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				versions, err := store.ListPackageVersions(ctx, instance.ID, "target")
				if err != nil || len(versions) != min(count, 3) {
					t.Fatalf("retained versions: %#v, error=%v", versions, err)
				}
				for i, version := range versions {
					if version.ID != fmt.Sprintf("version-%d", count-1-i) {
						t.Fatalf("unexpected retained version: %#v", version)
					}
				}
				other, err := store.ListPackageVersions(ctx, instance.ID, "other")
				if err != nil || len(other) != 1 {
					t.Fatalf("pruning touched another package: %#v, error=%v", other, err)
				}
			}
		})
	}
}
