package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestSmartPreviewRestoresAuthenticatedInputsAfterRestart(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	root := filepath.Join(e.config.DataDir, smartPreviewDirectory)
	raw, err := os.ReadFile(filepath.Join(root, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{e.config.Credential, instance, job.Payload["content_base64"].(string), "target_path", "registered_paths"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("persistent preview leaked credentials, content, host paths or cached target state")
		}
	}
	e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
	if err := e.cleanupSmartPreviews(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(e.smartPreviews) != 1 || e.smartPreviews[id] == nil {
		t.Fatal("ready preview was not recovered")
	}
	used := false
	if _, err := e.Execute(context.Background(), job, func(_ int, message string) { used = used || strings.Contains(message, "复用预检") }); err != nil || !used {
		t.Fatalf("restored inputs not used: %v %v", used, err)
	}
	assertFileContent(t, filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx"), "smx-content")
	assertSmartPreviewEmpty(t, root)
}

func assertSmartPreviewEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("preview staging not empty: %v %v", entries, err)
	}
}

func TestSmartPreviewRestartRejectsUnsafeMetadataAndReclaimsOrphans(t *testing.T) {
	for _, scenario := range []string{"binding", "expiry", "claimed", "source", "digest", "unknown", "trailing", "truncated", "oversized", "rotated", "missing", "expired", "future", "interrupted", "orphan"} {
		t.Run(scenario, func(t *testing.T) {
			e, instance, job, id := smartPreviewFixture(t)
			root := filepath.Join(e.config.DataDir, smartPreviewDirectory)
			meta := filepath.Join(root, id+".json")
			record, err := readSmartPreviewRecord(root, id, e.config.Credential)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(record)
			switch scenario {
			case "binding":
				record.RequestKey = strings.Repeat("a", 64)
			case "expiry":
				record.ExpiresAt = record.ExpiresAt.Add(time.Hour)
			case "claimed":
				record.Claimed = true // Tampering with state cannot produce a valid MAC.
			case "source":
				record.Files[0].Source = "../outside"
			case "digest":
				record.Files[0].SHA256 = strings.Repeat("b", 64)
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(",\"target\":\"../outside\"}")...)
			case "trailing":
				raw = append(raw, []byte(" {}")...)
			case "truncated":
				raw = raw[:len(raw)/2]
			case "oversized":
				raw = []byte(strings.Repeat(" ", smartPreviewMetadataLimit+1))
			case "rotated":
				e.config.Credential = "rotated-test-credential"
			case "missing":
				e.config.Credential = ""
			case "expired":
				record.CreatedAt = time.Now().Add(-2 * time.Hour)
				record.ExpiresAt = record.CreatedAt.Add(smartPreviewTTL)
				record.MAC = smartPreviewMAC(record, e.config.Credential)
			case "future":
				record.CreatedAt = time.Now().Add(time.Hour)
				record.ExpiresAt = record.CreatedAt.Add(smartPreviewTTL)
				record.MAC = smartPreviewMAC(record, e.config.Credential)
			case "interrupted":
				record.Claimed = true
				record.MAC = smartPreviewMAC(record, e.config.Credential)
			}
			if scenario != "unknown" && scenario != "trailing" && scenario != "truncated" && scenario != "oversized" {
				raw, _ = json.Marshal(record)
			}
			if err := os.WriteFile(meta, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "orphan" {
				if err := os.Remove(meta); err != nil {
					t.Fatal(err)
				}
			}
			e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			payload, _ := decodeOperation(job)
			if _, hit, err := e.installSmartPreview(context.Background(), job, payload, instance, func(int, string) {}); err != nil || hit {
				t.Fatalf("rejected metadata was restored: %v %v", hit, err)
			}
			assertSmartPreviewEmpty(t, root)
			// A rejected optimization may fall back only to the ordinary archive
			// path with the original content, fingerprint and install validations.
			if _, err := e.Execute(context.Background(), job, nil); err != nil {
				t.Fatalf("original verified archive fallback failed: %v", err)
			}
		})
	}
}

func TestSmartPreviewClaimIsDurableBeforeAnyInstanceWrite(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	root := filepath.Join(e.config.DataDir, smartPreviewDirectory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.Execute(ctx, job, func(progress int, _ string) {
		if progress != 20 {
			return
		}
		record, err := readSmartPreviewRecord(root, id, e.config.Credential)
		if err != nil || !record.Claimed {
			t.Fatalf("claim was not durable before consumption: %+v %v", record, err)
		}
		if _, err := os.Stat(filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx")); !os.IsNotExist(err) {
			t.Fatal("instance was written before durable claim")
		}
		cancel()
	})
	if err != context.Canceled {
		t.Fatalf("cancellation was lost: %v", err)
	}
	assertSmartPreviewEmpty(t, root)
}

func TestSmartPreviewManifestValidationAndFixedNames(t *testing.T) {
	e, _, _, id := smartPreviewFixture(t)
	root := filepath.Join(e.config.DataDir, smartPreviewDirectory)
	original, err := readSmartPreviewRecord(root, id, e.config.Credential)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"id", "version", "ttl", "format", "encoding", "archive", "empty", "count", "size", "total", "duplicate", "traversal", "device", "ads"} {
		t.Run(scenario, func(t *testing.T) {
			record := original
			record.Files = append([]smartPreviewInput(nil), original.Files...)
			switch scenario {
			case "id":
				record.ID = uuid.NewString()
			case "version":
				record.Version++
			case "ttl":
				record.ExpiresAt = record.ExpiresAt.Add(time.Second)
			case "format":
				record.Format = "exe"
			case "encoding":
				record.PathEncoding = "invalid"
			case "archive":
				record.ArchiveName = "../x.zip"
			case "empty":
				record.Files = nil
			case "count":
				record.Files = make([]smartPreviewInput, maxSmartPluginFiles+1)
			case "size":
				record.Files[0].Size = maxSmartPluginEntry + 1
			case "total":
				record.ExpandedBytes++
			case "duplicate":
				record.Files = append(record.Files, record.Files[0])
			case "traversal":
				record.Files[0].Source = "../x.smx"
			case "device":
				record.Files[0].Source = "CON.smx"
			case "ads":
				record.Files[0].Source = "x.smx:stream"
			}
			if err := record.validate(id); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	for _, name := range []string{"../" + id + ".json", id + "-0000.bin", id + "-5001.bin", id + "-0001.bin:stream", id + "-0001.binx"} {
		if smartPreviewStagingName(name) {
			t.Fatalf("unsafe staging name accepted: %s", name)
		}
	}
}

func TestSmartPreviewCleansLegacyFilesWithoutFollowingLinks(t *testing.T) {
	e, _, _, _ := smartPreviewFixture(t)
	root, err := ensureDataSubdirectory(e.config.DataDir, "smart-previews")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, uuid.NewString()+".bin"), []byte("old cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupLegacySmartPreviews(); err != nil {
		t.Fatal(err)
	}
	assertSmartPreviewEmpty(t, root)
	// Unknown files must not be removed by a broad directory cleanup.
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupLegacySmartPreviews(); err == nil {
		t.Fatal("unknown legacy entry accepted")
	}
	assertFileContent(t, filepath.Join(root, "keep.txt"), "keep")
}

func TestSmartPreviewRestoredInputsStillRequireInstallAcknowledgements(t *testing.T) {
	for _, scenario := range []string{"native", "takeover"} {
		t.Run(scenario, func(t *testing.T) {
			e, _, instance := uploadedPackageExecutor(t)
			e.config.Credential = "test-credential"
			e.locks = newKeyedLocker()
			source := "addons/sourcemod/extensions/sample.dll"
			if scenario == "takeover" {
				source = "addons/sourcemod/plugins/sample.smx"
				if err := os.MkdirAll(filepath.Join(instance, "left4dead2", filepath.Dir(source)), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(instance, "left4dead2", source), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			job := domain.Job{ID: "inspection", Type: "inspect_plugin_archive", InstanceID: "instance", Payload: map[string]any{
				"name": "sample.zip", "content_base64": base64.StdEncoding.EncodeToString(smartTestZIP(t, map[string]string{source: "new"})),
			}}
			result, err := e.Execute(context.Background(), job, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(e.smartPreviews) == 0 {
				t.Skip("preview disk reserve unavailable")
			}
			e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
			job.ID, job.Type = "install", "install_package"
			job.Payload["kind"] = "smart"
			job.Payload["inspection_fingerprint"] = result["fingerprint"]
			job.Payload["smart_packages"] = []any{map[string]any{"folder_id": result["folders"].([]any)[0].(map[string]any)["id"]}}
			used := false
			if _, err := e.Execute(context.Background(), job, func(_ int, message string) { used = used || strings.Contains(message, "复用预检") }); err == nil || !used {
				t.Fatalf("restored cache bypassed acknowledgement: used=%v err=%v", used, err)
			}
			if scenario == "takeover" {
				assertFileContent(t, filepath.Join(instance, "left4dead2", source), "old")
			} else if _, err := os.Stat(filepath.Join(instance, "left4dead2", source)); !os.IsNotExist(err) {
				t.Fatal("unacknowledged native code was written")
			}
		})
	}
}

func TestSmartPreviewClaimFailureDoesNotModifyInstance(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	meta := filepath.Join(e.config.DataDir, smartPreviewDirectory, id+".json")
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(meta, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), job, nil); err == nil {
		t.Fatal("installation proceeded without durable claim")
	}
	if _, err := os.Stat(filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx")); !os.IsNotExist(err) {
		t.Fatal("failed claim wrote instance files")
	}
}

func TestSmartPreviewRejectsLinkedMetadata(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	root := filepath.Join(e.config.DataDir, smartPreviewDirectory)
	meta := filepath.Join(root, id+".json")
	raw, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep.json")
	if err := os.WriteFile(outside, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, meta); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readSmartPreviewRecord(root, id, e.config.Credential); err == nil {
		t.Fatal("linked metadata was read")
	}
	e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
	payload, _ := decodeOperation(job)
	if _, hit, _ := e.installSmartPreview(context.Background(), job, payload, instance, func(int, string) {}); hit {
		t.Fatal("linked metadata was restored")
	}
	assertFileContent(t, outside, string(raw))
}
