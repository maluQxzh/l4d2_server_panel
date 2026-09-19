package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func preparedExecutor(t *testing.T) (*Executor, string, string) {
	e, data, instance := uploadedPackageExecutor(t)
	e.config.NodeID = "node"
	e.locks = newKeyedLocker()
	return e, data, instance
}

func prepareTestPackage(t *testing.T, e *Executor) (domain.Job, map[string]any) {
	t.Helper()
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "vpk", "name": "example.vpk", "content_base64": base64.StdEncoding.EncodeToString(testVPK('B'))}}
	result, err := e.Execute(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	return job, result
}

func preparedInstallJob(job domain.Job, result map[string]any) domain.Job {
	return domain.Job{ID: uuid.NewString(), NodeID: job.NodeID, InstanceID: job.InstanceID, Type: "install_mod", Payload: map[string]any{"prepared_package_id": job.ID, "name": "example.vpk", "kind": "vpk", "expected_sha256": result["sha256"], "expected_size": result["size"]}}
}

func TestPreparedPackagesParallelWhileInstanceLocked(t *testing.T) {
	e, _, instance := preparedExecutor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := e.locks.acquire(ctx, "instance")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	entered := make(chan struct{}, 3)
	finish := make(chan struct{})
	e.packageDownloader = func(ctx context.Context, _, dest string, official bool) (string, int64, error) {
		entered <- struct{}{}
		select {
		case <-finish:
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
		err := os.WriteFile(dest, testVPK('X'), 0600)
		return "", 0, err
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Execute(ctx, domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "vpk", "name": "sample.vpk", "source": "https://example.com/sample.vpk"}}, nil)
			errorsOut <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("downloads were serialized by instance lock")
		}
	}
	select {
	case <-entered:
		t.Error("more than two preparations ran concurrently")
	default:
	}
	close(finish)
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(instance, "left4dead2", "addons", "sample.vpk")); !os.IsNotExist(err) {
		t.Fatal("preparation wrote instance files")
	}
}

func TestPreparedPackageRestartInstallAndRollback(t *testing.T) {
	e, data, instance := preparedExecutor(t)
	installUploadedVersion(t, e, "old", "example.vpk", "vpk", testVPK('A'))
	job, result := prepareTestPackage(t, e)
	// A fresh executor's cleanup retains a ready, unclaimed durable package.
	e.preparedActive = nil
	if err := e.cleanupPreparedPackages(time.Now()); err != nil {
		t.Fatal(err)
	}
	installed, err := e.Execute(context.Background(), preparedInstallJob(job, result), nil)
	if err != nil {
		t.Fatal(err)
	}
	if installed["previous_version"] == nil {
		t.Fatal("prepared install bypassed version snapshots")
	}
	if _, err := os.Stat(filepath.Join(data, "prepared-packages", job.ID+".bin")); !os.IsNotExist(err) {
		t.Fatal("consumed payload retained")
	}
	job, result = prepareTestPackage(t, e)
	e.packageMutationVerifier = func(string, string) error { return errors.New("simulated failure") }
	if _, err := e.Execute(context.Background(), preparedInstallJob(job, result), nil); err == nil {
		t.Fatal("mutation failure ignored")
	}
	raw, err := os.ReadFile(filepath.Join(instance, "left4dead2", "addons", "example.vpk"))
	if err != nil || string(raw) != string(testVPK('B')) {
		t.Fatal("failed mutation did not restore previous payload")
	}
}

func TestPreparedPackageBindingExpiryTamperAndDiscard(t *testing.T) {
	for _, scenario := range []string{"instance", "node", "hash", "size", "expiry", "tamper", "discard", "link"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			job, result := prepareTestPackage(t, e)
			install := preparedInstallJob(job, result)
			root := filepath.Join(data, "prepared-packages")
			switch scenario {
			case "instance":
				install.InstanceID = "other"
			case "node":
				install.NodeID = "other"
			case "hash":
				install.Payload["expected_sha256"] = "bad"
			case "size":
				install.Payload["expected_size"] = 1
			case "expiry":
				record, err := readPreparedPackage(root, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				record.ExpiresAt = time.Now().Add(-time.Minute)
				if err := savePreparedPackage(root, record); err != nil {
					t.Fatal(err)
				}
			case "tamper":
				if err := os.WriteFile(filepath.Join(root, job.ID+".bin"), testVPK('C'), 0600); err != nil {
					t.Fatal(err)
				}
			case "discard":
				install.Type = "discard_prepared_package"
			case "link":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, testVPK('B'), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, job.ID+".bin")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, job.ID+".bin")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			_, err := e.Execute(context.Background(), install, nil)
			if scenario == "discard" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe prepared commit accepted")
			}
			if _, err := os.Stat(filepath.Join(instance, "left4dead2", "addons", "example.vpk")); !os.IsNotExist(err) {
				t.Fatal("rejected operation changed instance")
			}
		})
	}
}

func TestPreparedPackageCleanupAndCancellation(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	job, _ := prepareTestPackage(t, e)
	root := filepath.Join(data, "prepared-packages")
	record, err := readPreparedPackage(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Ready = false
	if err := savePreparedPackage(root, record); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupPreparedPackages(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, job.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("crashed reservation retained")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.packageDownloader = func(context.Context, string, string, bool) (string, int64, error) {
		cancel()
		return "", 0, context.Canceled
	}
	job.ID = uuid.NewString()
	job.Payload = map[string]any{"kind": "vpk", "name": "file.vpk", "source": "https://example.com/file.vpk"}
	if _, err := e.Execute(ctx, job, nil); err == nil {
		t.Fatal("canceled preparation succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, job.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("canceled reservation retained")
	}
}

func TestPreparedPluginPreservesBytesNameAndRisk(t *testing.T) {
	for _, kind := range []string{"smx", "native"} {
		t.Run(kind, func(t *testing.T) {
			e, _, instance := preparedExecutor(t)
			name := "MixedCase.smx"
			if kind == "native" {
				name = "MixedCase.so"
			}
			body := []byte("CaseSensitive plugin payload\x00\xff")
			job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": kind, "name": name, "content_base64": base64.StdEncoding.EncodeToString(body)}}
			result, err := e.Execute(context.Background(), job, nil)
			if err != nil {
				t.Fatal(err)
			}
			install := preparedInstallJob(job, result)
			install.Payload["kind"], install.Payload["name"] = kind, name
			if kind == "native" {
				if _, err := e.Execute(context.Background(), install, nil); err == nil {
					t.Fatal("native installed without risk acknowledgement")
				}
				job.ID = uuid.NewString()
				result, err = e.Execute(context.Background(), job, nil)
				if err != nil {
					t.Fatal(err)
				}
				install = preparedInstallJob(job, result)
				install.Payload["kind"], install.Payload["name"], install.Payload["acknowledge_native_risk"] = kind, name, true
			}
			if _, err := e.Execute(context.Background(), install, nil); err != nil {
				t.Fatal(err)
			}
			target, _ := domain.PreparedPackageTarget(kind, name)
			actual, err := os.ReadFile(filepath.Join(instance, filepath.FromSlash(target)))
			if err != nil || string(actual) != string(body) {
				t.Fatalf("payload or name changed: %v", err)
			}
		})
	}
}

func TestPreparedPackageCapacityAndExpiredOrphans(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	job, _ := prepareTestPackage(t, e)
	root := filepath.Join(data, "prepared-packages")
	record, err := readPreparedPackage(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < domain.PreparedPackageSlots; i++ {
		record.ID = uuid.NewString()
		if err := savePreparedPackage(root, record); err != nil {
			t.Fatal(err)
		}
	}
	job.ID = uuid.NewString()
	if _, err := e.Execute(context.Background(), job, nil); err == nil {
		t.Fatal("capacity exceeded")
	}
	for _, filename := range []string{uuid.NewString() + ".bin", ".download-orphan", ".managed-orphan"} {
		if err := os.WriteFile(filepath.Join(root, filename), []byte("orphan"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.cleanupPreparedPackages(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired/orphan staging retained: %v %v", entries, err)
	}
	if _, err := e.Execute(context.Background(), job, nil); err != nil {
		t.Fatalf("capacity not recovered: %v", err)
	}
}
