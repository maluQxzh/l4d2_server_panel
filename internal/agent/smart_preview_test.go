package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func smartPreviewFixture(t *testing.T) (*Executor, string, domain.Job, string) {
	t.Helper()
	e, _, instance := uploadedPackageExecutor(t)
	e.locks = newKeyedLocker()
	e.config.Credential = "test-smart-preview-credential"
	encoded := base64.StdEncoding.EncodeToString(smartTestZIP(t, map[string]string{"sample.smx": "smx-content"}))
	job := domain.Job{ID: "inspection", Type: "inspect_plugin_archive", InstanceID: "instance", Payload: map[string]any{
		"name": "sample.zip", "content_base64": encoded,
	}}
	result, err := e.Execute(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for candidate := range e.smartPreviews {
		id = candidate
	}
	if id == "" {
		t.Skip("preview cache disabled by its disk reserve requirement")
	}
	job.ID, job.Type = "install", "install_package"
	job.Payload["kind"] = "smart"
	job.Payload["inspection_fingerprint"] = result["fingerprint"]
	job.Payload["smart_packages"] = []any{map[string]any{"folder_id": result["folders"].([]any)[0].(map[string]any)["id"]}}
	return e, instance, job, id
}

func TestSmartPreviewReusesPreparationAndCleansConsumedFiles(t *testing.T) {
	e, instance, job, _ := smartPreviewFixture(t)
	used := false
	if _, err := e.Execute(context.Background(), job, func(_ int, message string) {
		used = used || strings.Contains(message, "复用预检")
	}); err != nil {
		t.Fatal(err)
	}
	if !used || len(e.smartPreviews) != 0 {
		t.Fatalf("cache was not consumed: used=%v records=%d", used, len(e.smartPreviews))
	}
	assertFileContent(t, filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx"), "smx-content")
	entries, err := os.ReadDir(filepath.Join(e.config.DataDir, smartPreviewDirectory))
	if err != nil || len(entries) != 0 {
		t.Fatalf("consumed files remain: %v %v", entries, err)
	}
}

func TestSmartPreviewExplicitHandleInstallsWithoutOriginalSource(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	job.Payload["smart_preview_id"] = id
	delete(job.Payload, "content_base64")
	delete(job.Payload, "source")
	delete(job.Payload, "name")
	used := false
	if _, err := e.Execute(context.Background(), job, func(_ int, message string) {
		used = used || strings.Contains(message, "复用预检")
	}); err != nil || !used {
		t.Fatalf("explicit preview handle was not consumed: used=%v err=%v", used, err)
	}
	assertFileContent(t, filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx"), "smx-content")
}

func TestSmartPreviewQQSourceBindingAndActiveCleanup(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	payload, err := decodeOperation(job)
	if err != nil {
		t.Fatal(err)
	}
	// The cached tree is transport-independent. Bind it as the result of a QQ
	// inspection; consumption must use its verified name and never fetch a URL.
	payload.Name, payload.ContentBase64 = "", ""
	payload.SourceTransport, payload.Source = "qq_flash", "https://qfile.qq.com/q/share01"
	e.smartPreviews[id].requestKey = smartPreviewRequestKey(job, payload, instance)
	record := e.smartPreviews[id].record
	record.RequestKey = e.smartPreviews[id].requestKey
	if err := saveSmartPreviewRecord(filepath.Join(e.config.DataDir, smartPreviewDirectory), record, e.config.Credential); err != nil {
		t.Fatal(err)
	}
	e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
	changed := payload
	changed.Source = "https://qfile.qq.com/q/share02"
	if _, hit, err := e.installSmartPreview(context.Background(), job, changed, instance, func(int, string) {}); hit || err != nil {
		t.Fatalf("different QQ source reused the cache: %v %v", hit, err)
	}
	var cleanupErr error
	_, hit, err := e.installSmartPreview(context.Background(), job, payload, instance, func(progress int, _ string) {
		if progress == 20 {
			// A pinned consumer survives expiry while hashing/committing. Cleanup
			// cannot remove its input, even when called from another goroutine.
			done := make(chan error, 1)
			go func() { done <- e.cleanupSmartPreviews(time.Now().Add(2 * time.Hour)) }()
			cleanupErr = <-done
		}
	})
	if err != nil || !hit || cleanupErr != nil {
		t.Fatalf("QQ preview consumption failed: hit=%v err=%v cleanup=%v", hit, err, cleanupErr)
	}
	assertFileContent(t, filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx"), "smx-content")
}

func TestSmartPreviewRejectsChangedContentTargetAndOwnership(t *testing.T) {
	for _, scenario := range []string{"content", "same-size-content", "missing", "link", "target", "ownership", "commit"} {
		t.Run(scenario, func(t *testing.T) {
			e, instance, job, id := smartPreviewFixture(t)
			cachePath := filepath.Join(e.config.DataDir, smartPreviewDirectory, e.smartPreviews[id].sources["sample.smx"])
			target := filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx")
			switch scenario {
			case "content":
				if err := os.WriteFile(cachePath, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "same-size-content":
				if err := os.WriteFile(cachePath, []byte("bad-content"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
			case "link":
				outside := filepath.Join(t.TempDir(), "outside.smx")
				if err := os.WriteFile(outside, []byte("smx-content"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, cachePath); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "target":
				if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("new owner"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ownership":
				job.Payload["registered_paths"] = map[string]string{"left4dead2/addons/sourcemod/plugins/sample.smx": "another-package"}
			}
			e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			_, err := e.Execute(context.Background(), job, func(progress int, _ string) {
				if scenario == "commit" && progress == 60 {
					if err := os.WriteFile(cachePath, []byte("changed after validation"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err == nil {
				t.Fatal("changed preview was installed")
			}
			if scenario == "target" {
				assertFileContent(t, target, "new owner")
			} else if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("rejected preview changed the instance: %v", err)
			}
		})
	}
}

func TestSmartPreviewMissBindingExpiryRestartAndCapacity(t *testing.T) {
	for _, scenario := range []string{"instance", "node", "path", "source", "encoding", "fingerprint", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			e, instance, job, id := smartPreviewFixture(t)
			payload, err := decodeOperation(job)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "expired" {
				e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			}
			switch scenario {
			case "instance":
				job.InstanceID = "another"
			case "node":
				job.NodeID = "another"
			case "path":
				instance = t.TempDir()
			case "source":
				payload.ContentBase64 += "changed"
			case "encoding":
				payload.PathEncoding = "gbk"
			case "fingerprint":
				payload.InspectionFingerprint = strings.Repeat("0", 64)
			case "expired":
				e.smartPreviews[id].expires = time.Now().Add(-time.Second)
			}
			if _, hit, err := e.installSmartPreview(context.Background(), job, payload, instance, func(int, string) {}); err != nil || hit {
				t.Fatalf("unexpected cache hit: %v %v", hit, err)
			}
			if scenario == "expired" {
				// Full API still installs from the original archive after a miss.
				if _, err := e.Execute(context.Background(), job, nil); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(filepath.Join(e.config.DataDir, smartPreviewDirectory))
				if err != nil || len(entries) != 0 {
					t.Fatalf("expired/orphan data remains: %v %v", entries, err)
				}
			}
		})
	}
	e, _, _, _ := smartPreviewFixture(t)
	e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
	var reservations []string
	for i := 1; i < smartPreviewSlots; i++ {
		id, err := e.reserveSmartPreview()
		if err != nil {
			t.Fatalf("reservation failed: %s %v", id, err)
		}
		if id == "" {
			t.Skip("insufficient disk reserve for all four cache slots")
		}
		reservations = append(reservations, id)
	}
	if id, err := e.reserveSmartPreview(); err != nil || id != "" {
		t.Fatalf("slot limit ignored: %s %v", id, err)
	}
	for _, id := range reservations {
		e.finishSmartPreview(id)
	}
	if len(e.smartPreviews) != 1 {
		t.Fatal("failed preparations were retained")
	}
}

func TestSmartPreviewCancellationDoesNotWriteInstance(t *testing.T) {
	e, instance, job, _ := smartPreviewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.Execute(ctx, job, func(progress int, _ string) {
		if progress == 20 {
			cancel()
		}
	})
	if err != context.Canceled {
		t.Fatalf("canceled preparation was committed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx")); !os.IsNotExist(err) {
		t.Fatalf("cancellation wrote the instance: %v", err)
	}
	if len(e.smartPreviews) != 0 {
		t.Fatal("canceled consumption kept its cache")
	}
}

func TestSmartPreviewInspectionSharesBoundedPreparationSlotsWithoutWriteLock(t *testing.T) {
	e, _, _, _ := smartPreviewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseWrite, err := e.locks.acquire(ctx, "instance")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseWrite()
	var releases []func()
	for i := 0; i < 2; i++ {
		release, err := e.acquirePreparationSlot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	job := domain.Job{ID: "another-inspect", Type: "inspect_plugin_archive", InstanceID: "instance", Payload: map[string]any{
		"name": "other.zip", "content_base64": base64.StdEncoding.EncodeToString(smartTestZIP(t, map[string]string{"other.smx": "other"})),
	}}
	finished := make(chan error, 1)
	go func() { _, err := e.Execute(ctx, job, nil); finished <- err }()
	select {
	case err := <-finished:
		t.Fatalf("third preparation did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	releases[0]()
	defer releases[1]()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inspection waited for the instance write lock")
	}
	// Concurrent retention passes must remain race-free.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.cleanupSmartPreviews(time.Now()) }()
	}
	wg.Wait()
}
