package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestStageQQFlashPluginPackageDownloadsOneFile(t *testing.T) {
	const filesetID = "8f877f3c-dab9-4b4d-90a5-673076efb936"
	const body = "plugin archive"
	transport := qqRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Host == "qfile.qq.com" && strings.HasPrefix(request.URL.Path, "/q/"):
			headers := make(http.Header)
			headers.Add("Set-Cookie", "deviceId=test-device; Path=/")
			return qqResponse(http.StatusOK, qqSharePage(t, filesetID, 1), headers), nil
		case strings.HasSuffix(request.URL.Path, qqFlashListRPC):
			return qqResponse(http.StatusOK, `{"retcode":0,"error":{"code":0},"data":{"file_lists":[{"is_end":true,"pagination_info":"done","parent_id":"","file_list":[{"name":"plugins.zip","cli_fileid":"file-a","is_dir":false,"file_physical_size":"14","safe_status":3,"physical":{"id":"physical-a","status":2,"download_limit_status":0}}]}]}}`, nil), nil
		case strings.HasSuffix(request.URL.Path, qqFlashDownloadRPC):
			var payload qqBatchDownloadRequest
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			return qqResponse(http.StatusOK, `{"retcode":0,"error":{"code":0},"data":{"download_rsp":[{"url":"https://multimedia.qfile.qq.com/download?id=physical-a","ret_code":"0"}]}}`, nil), nil
		case request.URL.Host == "multimedia.qfile.qq.com":
			return qqResponse(http.StatusOK, body, nil), nil
		default:
			t.Fatalf("unexpected QQ request: %s", request.URL)
			return nil, nil
		}
	})
	client := &qqFlashClient{metadata: &http.Client{Transport: transport}, download: &http.Client{Transport: transport}}
	executor := &Executor{config: &Config{DataDir: t.TempDir()}}
	path, name, digest, size, cleanup, err := executor.stageQQFlashPluginPackageWithClient(context.Background(), client, "job", "https://qfile.qq.com/q/share01", func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != body || name != "plugins.zip" || digest == "" || size != int64(len(body)) {
		t.Fatalf("staged QQ plugin package = name %q digest %q size %d body %q err %v", name, digest, size, raw, err)
	}
}

func TestQQPluginPackageExtension(t *testing.T) {
	for name, want := range map[string]string{"bundle.TAR.GZ": ".tar.gz", "plugin.smx": ".smx", "unknown.bin": ".package"} {
		if got := qqPluginPackageExtension(name); got != want {
			t.Fatalf("qqPluginPackageExtension(%q) = %q, want %q", name, got, want)
		}
	}
}
