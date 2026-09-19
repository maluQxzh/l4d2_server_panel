package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func preparedQQTestClient(t *testing.T, e *Executor, names []string, size int64, body []byte, downloaded *int) *qqFlashClient {
	t.Helper()
	transport := qqRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "qfile.qq.com" && strings.HasPrefix(req.URL.Path, "/q/"):
			headers := make(http.Header)
			headers.Add("Set-Cookie", "deviceId=test-device; Path=/")
			return qqResponse(http.StatusOK, qqSharePage(t, "8f877f3c-dab9-4b4d-90a5-673076efb936", int64(len(names))), headers), nil
		case strings.HasSuffix(req.URL.Path, qqFlashListRPC):
			files := []map[string]any{}
			for i, name := range names {
				files = append(files, map[string]any{"name": name, "cli_fileid": fmt.Sprintf("file-%d", i), "is_dir": false, "file_physical_size": fmt.Sprint(size), "safe_status": 3, "physical": map[string]any{"id": fmt.Sprintf("physical-%d", i), "status": 2, "download_limit_status": 0}})
			}
			raw, _ := json.Marshal(map[string]any{"retcode": 0, "error": map[string]any{"code": 0}, "data": map[string]any{"file_lists": []any{map[string]any{"is_end": true, "pagination_info": "done", "parent_id": "", "file_list": files}}}})
			return qqResponse(http.StatusOK, string(raw), nil), nil
		case strings.HasSuffix(req.URL.Path, qqFlashDownloadRPC):
			return qqResponse(http.StatusOK, `{"retcode":0,"error":{"code":0},"data":{"download_rsp":[{"url":"https://multimedia.qfile.qq.com/download?id=physical-0","ret_code":"0"}]}}`, nil), nil
		case req.URL.Host == "multimedia.qfile.qq.com":
			*downloaded++
			return qqResponse(http.StatusOK, string(body), nil), nil
		default:
			t.Fatalf("unexpected QQ request: %s", req.URL.Host)
			return nil, nil
		}
	})
	return &qqFlashClient{metadata: &http.Client{Transport: transport}, download: &http.Client{Transport: transport}, downloader: newSafeSegmentedDownloader(e.safeDownloadLimiter())}
}

func TestPreparedQQFlashDownloadAndInstall(t *testing.T) {
	for _, kind := range []string{"vpk", "smx", "native"} {
		t.Run(kind, func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			name, body := "MixedCase.vpk", testVPK('Q')
			if kind == "smx" {
				name, body = "MixedCase.smx", []byte("SMX bytes")
			}
			if kind == "native" {
				name, body = "MixedCase.so", []byte("native bytes")
			}
			downloaded := 0
			e.preparedQQClient = preparedQQTestClient(t, e, []string{name}, int64(len(body)), body, &downloaded)
			job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": kind, "source": "https://qfile.qq.com/q/SecretCASE", "source_transport": "qq_flash"}}
			unlock, err := e.locks.acquire(context.Background(), "instance")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := e.Execute(ctx, job, func(percent int, _ string) {
				if percent == 10 {
					record, err := readPreparedPackage(filepath.Join(data, "prepared-packages"), job.ID)
					if err != nil || record.Ready {
						t.Fatalf("invalid v1-compatible reservation: %v", err)
					}
				}
			})
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if result["name"] != name || downloaded == 0 {
				t.Fatalf("invalid preparation: %#v", result)
			}
			target, _ := domain.PreparedPackageTarget(kind, name)
			if _, err := os.Stat(filepath.Join(instance, filepath.FromSlash(target))); !os.IsNotExist(err) {
				t.Fatal("preparation wrote instance")
			}
			if err := e.cleanupPreparedPackages(time.Now()); err != nil {
				t.Fatal(err)
			}
			install := preparedInstallJob(job, result)
			install.Payload["name"], install.Payload["kind"], install.Payload["acknowledge_native_risk"] = name, kind, true
			if _, err := e.Execute(context.Background(), install, nil); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(filepath.Join(instance, filepath.FromSlash(target)))
			if err != nil || string(actual) != string(body) {
				t.Fatalf("bad installation: %v", err)
			}
			if _, err := os.Stat(filepath.Join(data, "prepared-packages", job.ID+".bin")); !os.IsNotExist(err) {
				t.Fatal("consumed bytes retained")
			}
		})
	}
}

func TestPreparedQQFlashRejectsInvalidSharesBeforeDownload(t *testing.T) {
	for _, test := range []struct {
		name  string
		files []string
		size  int64
	}{
		{"multiple", []string{"a.vpk", "b.vpk"}, 1},
		{"archive", []string{"bundle.zip"}, 1},
		{"kind", []string{"plugin.smx"}, 1},
		{"oversize", []string{"a.vpk"}, domain.PreparedPackageMaxBytes + 1},
		{"empty", []string{"a.vpk"}, 0},
		{"path", []string{"../a.vpk"}, 1},
		{"reserved", []string{"CON.vpk"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, data, _ := preparedExecutor(t)
			downloaded := 0
			e.preparedQQClient = preparedQQTestClient(t, e, test.files, test.size, []byte("unexpected"), &downloaded)
			job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "vpk", "source": "https://qfile.qq.com/q/SecretCASE", "source_transport": "qq_flash"}}
			if _, err := e.Execute(context.Background(), job, nil); err == nil {
				t.Fatal("invalid share accepted")
			}
			if downloaded != 0 {
				t.Fatal("invalid metadata downloaded bytes")
			}
			entries, err := os.ReadDir(filepath.Join(data, "prepared-packages"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed preparation leaked reservation: %v", err)
			}
		})
	}
}

func TestPreparedQQFlashRejectsCorruptVPKAndCleansReservation(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	downloaded := 0
	body := []byte("not a VPK")
	e.preparedQQClient = preparedQQTestClient(t, e, []string{"broken.vpk"}, int64(len(body)), body, &downloaded)
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "vpk", "source": "https://qfile.qq.com/q/share01", "source_transport": "qq_flash"}}
	if _, err := e.Execute(context.Background(), job, nil); err == nil || !strings.Contains(err.Error(), "VPK") {
		t.Fatalf("corrupt VPK accepted: %v", err)
	}
	if downloaded == 0 {
		t.Fatal("test did not validate downloaded bytes")
	}
	entries, err := os.ReadDir(filepath.Join(data, "prepared-packages"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected VPK leaked files: %v", err)
	}
}
