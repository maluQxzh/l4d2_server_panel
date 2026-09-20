package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func prepareTestWorkshop(t *testing.T, e *Executor, collection bool) (domain.Job, domain.PreparedWorkshop) {
	t.Helper()
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": domain.PreparedWorkshopKind, "name": domain.PreparedWorkshopName, "source": "123"}}
	steam := &workshopSteamOperations{files: map[string][]string{}}
	items := []string{"123"}
	if collection {
		items = []string{"10", "20"}
	}
	for _, id := range items {
		path := filepath.Join(t.TempDir(), "payload_legacy.bin")
		if err := os.WriteFile(path, []byte("opaque Workshop payload: "+id), 0600); err != nil {
			t.Fatal(err)
		}
		steam.files[id] = []string{path}
		if !collection {
			second := filepath.Join(filepath.Dir(path), "second.vpk")
			if err := os.WriteFile(second, []byte("second opaque payload"), 0600); err != nil {
				t.Fatal(err)
			}
			steam.files[id] = append(steam.files[id], second)
		}
	}
	e.steam = steam
	e.workshopCollectionResolver = func(context.Context, string) (bool, []string, error) { return collection, items, nil }
	e.workshopItemTitlesResolver = func(context.Context, []string) (map[string]string, error) {
		return map[string]string{"123": "Test.Title", "10": "First.Title", "20": "Second.Title"}, nil
	}
	result, err := e.Execute(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	var bundle domain.PreparedWorkshop
	raw, _ := json.Marshal(result)
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Files) != 2 || len(steam.calls) != len(items) {
		t.Fatal("unexpected Workshop snapshot")
	}
	return job, bundle
}

func workshopInstallJob(job domain.Job, bundle domain.PreparedWorkshop) domain.Job {
	return domain.Job{ID: uuid.NewString(), Type: "install_mod", NodeID: job.NodeID, InstanceID: job.InstanceID, Payload: map[string]any{"prepared_package_id": job.ID, "kind": bundle.Kind, "name": bundle.Name, "expected_sha256": bundle.SHA256, "expected_size": bundle.Size}}
}

func TestPreparedWorkshopArchivesParallelAndRestart(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(map[bool]string{false: "multiple", true: "archive"}[archive], func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			unlock, err := e.locks.acquire(context.Background(), "instance")
			if err != nil {
				t.Fatal(err)
			}
			job, bundle := prepareTestWorkshop(t, e, archive)
			unlock()
			if _, err := os.Stat(filepath.Join(instance, "left4dead2")); !os.IsNotExist(err) {
				t.Fatal("preparation wrote the instance")
			}
			e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			if err := e.cleanupPreparedWorkshops(time.Now()); err != nil {
				t.Fatal(err)
			}
			result, err := e.Execute(context.Background(), workshopInstallJob(job, bundle), nil)
			if err != nil || len(result["files"].([]map[string]any)) != 2 {
				t.Fatalf("consume after restart: %#v %v", result, err)
			}
			for _, file := range bundle.Files {
				target, _ := file.Target()
				sha, size, err := hashRegularFileWithinNoFollow(instance, target, domain.PreparedPackageMaxBytes)
				if err != nil || sha != file.SHA256 || size != file.Size {
					t.Fatalf("installed file: %s %d %v", sha, size, err)
				}
			}
			entries, err := os.ReadDir(filepath.Join(data, "prepared-workshop"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging not cleaned: %v %v", entries, err)
			}
			if _, err := e.Execute(context.Background(), workshopInstallJob(job, bundle), nil); err == nil {
				t.Fatal("bundle consumed twice")
			}
		})
	}
}

func TestPreparedWorkshopRejectsUnsafeConsumption(t *testing.T) {
	for _, scenario := range []string{"tamper", "manifest", "cross-instance", "expired", "link", "collision", "cancel", "commit-change"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			job, bundle := prepareTestWorkshop(t, e, true)
			install := workshopInstallJob(job, bundle)
			root := filepath.Join(data, "prepared-workshop")
			file := filepath.Join(root, preparedBundleFile(job.ID, 1))
			switch scenario {
			case "tamper":
				if err := os.WriteFile(file, testVPK('Z'), 0600); err != nil {
					t.Fatal(err)
				}
			case "manifest", "expired":
				record, err := readPreparedWorkshop(root, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "manifest" {
					record.Files[0].Name = "changed.vpk"
				} else {
					record.ExpiresAt = time.Now().Add(-time.Hour)
				}
				record.SHA256 = record.Digest()
				if err := savePreparedWorkshop(root, record); err != nil {
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
				target, _ := bundle.Files[1].Target()
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
				if scenario == "commit-change" && progress == 80 {
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
			first, _ := bundle.Files[0].Target()
			if _, err := os.Stat(filepath.Join(instance, filepath.FromSlash(first))); !os.IsNotExist(err) {
				t.Fatal("rejected bundle left partial install")
			}
			if scenario == "collision" {
				target, _ := bundle.Files[1].Target()
				assertFileContent(t, filepath.Join(instance, filepath.FromSlash(target)), "existing")
			}
		})
	}
}

func TestPreparedWorkshopDiscardAndExpiredCleanup(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	job, bundle := prepareTestWorkshop(t, e, false)
	job2, _ := prepareTestWorkshop(t, e, true)
	discard := workshopInstallJob(job, bundle)
	discard.Type = "discard_prepared_package"
	if _, err := e.Execute(context.Background(), discard, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupPreparedWorkshops(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparedWorkshop(filepath.Join(data, "prepared-workshop"), job2.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired cache retained: %v", err)
	}
}

func TestPreparedWorkshopInterruptedReservationAndClaimCleanup(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		e, data, _ := preparedExecutor(t)
		job, _ := prepareTestWorkshop(t, e, true)
		root := filepath.Join(data, "prepared-workshop")
		record, err := readPreparedWorkshop(root, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if claimed {
			record.Claim = "interrupted"
		} else {
			record.Ready = false
			record.Files = nil
			record.SHA256 = ""
			record.Size = 0
		}
		if err := savePreparedWorkshop(root, record); err != nil {
			t.Fatal(err)
		}
		e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
		if err := e.cleanupPreparedWorkshops(time.Now()); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("interrupted staging retained: %v %v", entries, err)
		}
	}
}

func TestPreparedWorkshopRejectsInvalidSourcesBeforeDownload(t *testing.T) {
	for _, scenario := range []string{"url", "command", "upload", "transport", "name", "node"} {
		e, _, _ := preparedExecutor(t)
		steam := &workshopSteamOperations{}
		e.steam = steam
		job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "workshop", "name": domain.PreparedWorkshopName, "source": "123"}}
		switch scenario {
		case "url":
			job.Payload["source"] = "https://example.com/123"
		case "command":
			job.Payload["source"] = "123 +quit"
		case "upload":
			job.Payload["content_base64"] = "YQ=="
		case "transport":
			job.Payload["source_transport"] = "qq_flash"
		case "name":
			job.Payload["name"] = "caller.vpk"
		case "node":
			job.NodeID = "another"
		}
		if _, err := e.Execute(context.Background(), job, nil); err == nil {
			t.Fatalf("invalid source accepted: %s", scenario)
		}
		if len(steam.calls) != 0 {
			t.Fatal("invalid source reached SteamCMD")
		}
	}
}

func TestPreparedWorkshopLargeOpaquePayload(t *testing.T) {
	e, data, instance := preparedExecutor(t)
	job, bundle := prepareTestWorkshop(t, e, false)
	root := filepath.Join(data, "prepared-workshop")
	// Exercise the real consume path above the ordinary VPK cap with content
	// that deliberately has no VPK structure. The snapshot cap remains 8 GiB.
	file := filepath.Join(root, preparedBundleFile(job.ID, 0))
	if err := os.Truncate(file, (512<<20)+1); err != nil {
		t.Fatal(err)
	}
	sha, size, err := hashRegularFileNoFollow(file)
	if err != nil {
		t.Fatal(err)
	}
	record, err := readPreparedWorkshop(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Size += size - record.Files[0].Size
	record.Files[0].Size, record.Files[0].SHA256 = size, sha
	record.SHA256 = record.Digest()
	if err := savePreparedWorkshop(root, record); err != nil {
		t.Fatal(err)
	}
	bundle = record.PreparedWorkshop
	if _, err := e.Execute(context.Background(), workshopInstallJob(job, bundle), nil); err != nil {
		t.Fatal(err)
	}
	target, _ := bundle.Files[0].Target()
	info, err := os.Stat(filepath.Join(instance, target))
	if err != nil || info.Size() != size {
		t.Fatalf("large payload not installed: %v", err)
	}
}

func TestWorkshopDownloadLockIncludesSnapshotCopy(t *testing.T) {
	e, _, _ := preparedExecutor(t)
	e.steam = &workshopSteamOperations{files: map[string][]string{"123": {"unused"}}}
	e.workshopCollectionResolver = func(context.Context, string) (bool, []string, error) { return false, nil, nil }
	e.workshopItemTitlesResolver = func(context.Context, []string) (map[string]string, error) {
		return map[string]string{"123": "Title"}, nil
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.visitWorkshopDownloads(context.Background(), "123", func(int, string) {}, func(workshopDownload) error { close(entered); <-release; return nil })
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := e.visitWorkshopDownloads(ctx, "123", func(int, string) {}, func(workshopDownload) error { t.Error("snapshot overlapped same-item download"); return nil })
	close(release)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("same item did not wait: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPreparedWorkshopCanceledPreparationCleansCopiedInputs(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	// Set up a working fake source, then cancel during a second preparation.
	prepareTestWorkshop(t, e, true)
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "workshop", "name": domain.PreparedWorkshopName, "source": "123"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.Execute(ctx, job, func(_ int, message string) {
		if strings.Contains(message, "(2/2)") {
			cancel()
		}
	})
	if err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(data, "prepared-workshop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), job.ID) {
			t.Fatal("canceled preparation retained partial snapshot")
		}
	}
}

func TestPreparedWorkshopSharesPreparationSlotsAndBoundsStaging(t *testing.T) {
	e, _, _ := preparedExecutor(t)
	prepareTestWorkshop(t, e, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := e.acquirePreparationSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.acquirePreparationSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "workshop", "name": domain.PreparedWorkshopName, "source": "123"}}
	done := make(chan error, 1)
	go func() { _, err := e.Execute(ctx, job, nil); done <- err }()
	select {
	case err := <-done:
		first()
		t.Fatalf("preparation bypassed shared slots: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	first()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not resume")
	}
	t.Run("quota", func(t *testing.T) {
		root := filepath.Join(e.config.DataDir, "prepared-workshop")
		if err := ensureArchiveDiskReserve(root, 5*domain.PreparedWorkshopMaxBytes, 1<<30, "test"); err != nil {
			t.Skip("insufficient disk reserve to populate all four Workshop slots")
		}
		for i := 2; i < domain.PreparedWorkshopSlots; i++ {
			prepareTestWorkshop(t, e, false)
		}
		job.ID = uuid.NewString()
		if _, err := e.Execute(ctx, job, nil); err == nil {
			t.Fatal("Workshop snapshot quota exceeded")
		}
	})
}
