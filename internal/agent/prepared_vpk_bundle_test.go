package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func prepareTestVPKBundle(t *testing.T, e *Executor, archive bool) (domain.Job, domain.PreparedVPKBundle) {
	t.Helper()
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": domain.PreparedVPKBundleKind, "source_transport": "qq_flash", "source": "https://qfile.qq.com/q/share01"}}
	content, names := testVPK('B'), []string{"one.vpk", "two.vpk"}
	if archive {
		content = smartTestZIP(t, map[string]string{"one.vpk": string(testVPK('B')), "two.vpk": string(testVPK('C'))})
		names = []string{"bundle.zip"}
	}
	downloads := 0
	e.preparedQQClient = preparedQQTestClient(t, e, names, int64(len(content)), content, &downloads)
	result, err := e.Execute(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	var bundle domain.PreparedVPKBundle
	raw, _ := json.Marshal(result)
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Files) != 2 || downloads != len(names) {
		t.Fatalf("unexpected prepared result: %#v downloads=%d", bundle, downloads)
	}
	return job, bundle
}

func bundleInstallJob(job domain.Job, bundle domain.PreparedVPKBundle) domain.Job {
	return domain.Job{ID: uuid.NewString(), Type: "install_mod", NodeID: job.NodeID, InstanceID: job.InstanceID, Payload: map[string]any{"prepared_package_id": job.ID, "kind": bundle.Kind, "name": bundle.Name, "expected_sha256": bundle.SHA256, "expected_size": bundle.Size}}
}

func TestPreparedVPKBundleArchivesParallelAndRestart(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(map[bool]string{false: "multiple", true: "archive"}[archive], func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			unlock, err := e.locks.acquire(context.Background(), "instance")
			if err != nil {
				t.Fatal(err)
			}
			job, bundle := prepareTestVPKBundle(t, e, archive)
			unlock()
			if _, err := os.Stat(filepath.Join(instance, "left4dead2")); !os.IsNotExist(err) {
				t.Fatal("preparation wrote the instance")
			}
			e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			if err := e.cleanupPreparedVPKBundles(time.Now()); err != nil {
				t.Fatal(err)
			}
			result, err := e.Execute(context.Background(), bundleInstallJob(job, bundle), nil)
			if err != nil || result["files_imported"] != 2 {
				t.Fatalf("consume after restart: %#v %v", result, err)
			}
			for index, file := range bundle.Files {
				target, _ := domain.PreparedVPKBundleTarget(job.ID, index, file.Name)
				sha, size, err := hashRegularFileWithinNoFollow(instance, target, domain.PreparedPackageMaxBytes)
				if err != nil || sha != file.SHA256 || size != file.Size {
					t.Fatalf("installed file: %s %d %v", sha, size, err)
				}
			}
			entries, err := os.ReadDir(filepath.Join(data, "prepared-vpk-bundles"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging not cleaned: %v %v", entries, err)
			}
			if _, err := e.Execute(context.Background(), bundleInstallJob(job, bundle), nil); err == nil {
				t.Fatal("bundle consumed twice")
			}
		})
	}
}

func TestPreparedVPKBundleRejectsUnsafeConsumption(t *testing.T) {
	for _, scenario := range []string{"tamper", "manifest", "cross-instance", "expired", "link", "collision", "cancel", "commit-change"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			job, bundle := prepareTestVPKBundle(t, e, true)
			install := bundleInstallJob(job, bundle)
			root := filepath.Join(data, "prepared-vpk-bundles")
			file := filepath.Join(root, preparedBundleFile(job.ID, 1))
			switch scenario {
			case "tamper":
				if err := os.WriteFile(file, testVPK('Z'), 0600); err != nil {
					t.Fatal(err)
				}
			case "manifest", "expired":
				record, err := readPreparedVPKBundle(root, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "manifest" {
					record.Files[0].Name = "changed.vpk"
				} else {
					record.ExpiresAt = time.Now().Add(-time.Hour)
				}
				record.SHA256 = record.Digest()
				if err := savePreparedVPKBundle(root, record); err != nil {
					t.Fatal(err)
				}
			case "cross-instance":
				install.InstanceID = "another"
			case "link":
				outside := filepath.Join(t.TempDir(), "outside.vpk")
				if err := os.WriteFile(outside, testVPK('C'), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, file); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "collision":
				target, _ := domain.PreparedVPKBundleTarget(job.ID, 1, bundle.Files[1].Name)
				dest := filepath.Join(instance, filepath.FromSlash(target))
				if err := os.MkdirAll(filepath.Dir(dest), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := e.Execute(ctx, install, func(progress int, _ string) {
				if scenario == "commit-change" && progress == 72 {
					if err := os.WriteFile(file, testVPK('Z'), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "cancel" && progress >= 72 {
					cancel()
				}
			})
			if err == nil {
				t.Fatal("unsafe bundle installed")
			}
			first, _ := domain.PreparedVPKBundleTarget(job.ID, 0, bundle.Files[0].Name)
			if _, err := os.Stat(filepath.Join(instance, filepath.FromSlash(first))); !os.IsNotExist(err) {
				t.Fatal("rejected bundle left partial install")
			}
			if scenario == "collision" {
				target, _ := domain.PreparedVPKBundleTarget(job.ID, 1, bundle.Files[1].Name)
				assertFileContent(t, filepath.Join(instance, filepath.FromSlash(target)), "existing")
			}
		})
	}
}

func TestPreparedVPKBundleDiscardAndExpiredCleanup(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	job, bundle := prepareTestVPKBundle(t, e, false)
	job2, _ := prepareTestVPKBundle(t, e, true)
	discard := bundleInstallJob(job, bundle)
	discard.Type = "discard_prepared_package"
	if _, err := e.Execute(context.Background(), discard, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupPreparedVPKBundles(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedVPKBundle(filepath.Join(data, "prepared-vpk-bundles"), job2.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired cache retained: %v", err)
	}
}

func TestPreparedVPKBundleInvalidArchiveAndInterruptedCleanup(t *testing.T) {
	for _, scenario := range []string{"mixed", "corrupt", "traversal"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			files := map[string]string{"one.vpk": string(testVPK('B'))}
			switch scenario {
			case "mixed":
				files["plugin.smx"] = "not a VPK"
			case "corrupt":
				files["two.vpk"] = "invalid VPK"
			case "traversal":
				files["../outside.vpk"] = string(testVPK('B'))
			}
			body := smartTestZIP(t, files)
			downloads := 0
			e.preparedQQClient = preparedQQTestClient(t, e, []string{"bundle.zip"}, int64(len(body)), body, &downloads)
			job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": domain.PreparedVPKBundleKind, "source_transport": "qq_flash", "source": "https://qfile.qq.com/q/share01"}}
			if _, err := e.Execute(context.Background(), job, nil); err == nil {
				t.Fatal("invalid archive accepted")
			}
			entries, err := os.ReadDir(filepath.Join(data, "prepared-vpk-bundles"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed preparation leaked files: %v %v", entries, err)
			}
			if _, err := os.Stat(filepath.Join(instance, "left4dead2")); !os.IsNotExist(err) {
				t.Fatal("failed preparation wrote instance")
			}
		})
	}
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete", true: "claimed"}[claimed], func(t *testing.T) {
			e, data, _ := preparedExecutor(t)
			job, _ := prepareTestVPKBundle(t, e, true)
			root := filepath.Join(data, "prepared-vpk-bundles")
			record, err := readPreparedVPKBundle(root, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if claimed {
				record.Claim = uuid.NewString()
			} else {
				record.Ready = false
			}
			if err := savePreparedVPKBundle(root, record); err != nil {
				t.Fatal(err)
			}
			e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			if err := e.cleanupPreparedVPKBundles(time.Now()); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("interrupted files retained: %v %v", entries, err)
			}
		})
	}
}
