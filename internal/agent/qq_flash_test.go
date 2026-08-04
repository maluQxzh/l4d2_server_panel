package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type qqRoundTripFunc func(*http.Request) (*http.Response, error)

func (function qqRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func qqResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: headers,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

type nuxtTestBuilder struct {
	table []any
}

func (builder *nuxtTestBuilder) add(value any) int {
	index := len(builder.table)
	builder.table = append(builder.table, nil)
	switch item := value.(type) {
	case map[string]any:
		flat := make(map[string]any, len(item))
		builder.table[index] = flat
		for key, child := range item {
			flat[key] = builder.add(child)
		}
	case []any:
		flat := make([]any, len(item))
		builder.table[index] = flat
		for childIndex, child := range item {
			flat[childIndex] = builder.add(child)
		}
	default:
		builder.table[index] = value
	}
	return index
}

func qqSharePage(t *testing.T, filesetID string, totalFiles int64) string {
	t.Helper()
	root := map[string]any{"data": map[string]any{
		`trpc.file.FileFlashTrans/GetFileset_{"fileset_id":"` + filesetID + `"}`: map[string]any{
			"retcode": 0, "data": map[string]any{"fileset": map[string]any{
				"fileset_id": filesetID, "total_file_count": totalFiles, "expired_time": time.Now().Add(time.Hour).Unix(),
			}},
		},
	}}
	builder := &nuxtTestBuilder{table: []any{nil}}
	rootIndex := builder.add(root)
	builder.table[0] = []any{"ShallowReactive", rootIndex}
	raw, err := json.Marshal(builder.table)
	if err != nil {
		t.Fatal(err)
	}
	return `<html><script type="application/json" id="__NUXT_DATA__">` + string(raw) + `</script></html>`
}

func TestQQFlashImportsMultipleFilesAndFolders(t *testing.T) {
	filesetID := "8f877f3c-dab9-4b4d-90a5-673076efb936"
	contents := map[string]string{"physical-a": "alpha", "physical-b": "nested beta"}
	transport := qqRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Host == "qfile.qq.com" && strings.HasPrefix(request.URL.Path, "/q/"):
			headers := make(http.Header)
			headers.Add("Set-Cookie", "deviceId=test-device; Path=/")
			return qqResponse(http.StatusOK, qqSharePage(t, filesetID, 2), headers), nil
		case strings.HasSuffix(request.URL.Path, qqFlashListRPC):
			if request.Header.Get("x-qq-ar-signature") == "" || request.Header.Get("x-device-id") != "test-device" {
				t.Fatalf("QQ metadata request was not signed: %#v", request.Header)
			}
			var payload qqFileListRequest
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			parent := payload.RequestInfos[0].ParentID
			body := `{"retcode":0,"error":{"code":0},"data":{"file_lists":[{"is_end":true,"pagination_info":"done","parent_id":"","file_list":[{"name":"alpha.vpk","cli_fileid":"file-a","is_dir":false,"file_physical_size":"5","safe_status":3,"physical":{"id":"physical-a","status":2,"download_limit_status":0}},{"name":"folder","cli_fileid":"folder-id","is_dir":true,"file_physical_size":"0","safe_status":3,"physical":{}}]}]}}`
			if parent == "folder-id" {
				body = `{"retcode":0,"error":{"code":0},"data":{"file_lists":[{"is_end":true,"pagination_info":"done","parent_id":"folder-id","file_list":[{"name":"beta.txt","cli_fileid":"file-b","is_dir":false,"file_physical_size":"11","safe_status":3,"physical":{"id":"physical-b","status":2,"download_limit_status":0}}]}]}}`
			}
			return qqResponse(http.StatusOK, body, nil), nil
		case strings.HasSuffix(request.URL.Path, qqFlashDownloadRPC):
			var payload qqBatchDownloadRequest
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			physicalID := payload.DownloadInfo[0].BatchID
			body := `{"retcode":0,"error":{"code":0},"data":{"download_rsp":[{"url":"https://multimedia.qfile.qq.com/download?id=` + physicalID + `","ret_code":"0"}]}}`
			return qqResponse(http.StatusOK, body, nil), nil
		case request.URL.Host == "multimedia.qfile.qq.com":
			return qqResponse(http.StatusOK, contents[request.URL.Query().Get("id")], nil), nil
		default:
			t.Fatalf("unexpected QQ request: %s", request.URL)
			return nil, nil
		}
	})
	client := &qqFlashClient{metadata: &http.Client{Transport: transport}, download: &http.Client{Transport: transport}}
	instancePath := t.TempDir()
	progressValues := make([]int, 0)
	result, err := client.importFiles(context.Background(), instancePath, "https://qfile.qq.com/q/share01", func(value int, _ string) {
		progressValues = append(progressValues, value)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["files_imported"] != uint64(2) {
		t.Fatalf("unexpected import result: %#v", result)
	}
	for relative, expected := range map[string]string{
		"left4dead2/addons/alpha.vpk":       "alpha",
		"left4dead2/addons/folder/beta.txt": "nested beta",
	} {
		raw, err := os.ReadFile(filepath.Join(instancePath, filepath.FromSlash(relative)))
		if err != nil || string(raw) != expected {
			t.Fatalf("imported %s = %q, %v", relative, raw, err)
		}
	}
	if len(progressValues) < 3 || progressValues[len(progressValues)-1] != 100 {
		t.Fatalf("QQ flash progress was not reported: %#v", progressValues)
	}
	hasDownloadProgress := false
	for _, value := range progressValues {
		if value > 10 && value < 95 {
			hasDownloadProgress = true
			break
		}
	}
	if !hasDownloadProgress {
		t.Fatalf("QQ flash byte progress was not reported: %#v", progressValues)
	}
	if err := validateQQFlashFileMutation("left4dead2/addons/package.vpk"); err != nil {
		t.Fatalf("VPK should be accepted by QQ package imports: %v", err)
	}
	if err := validateQQFlashFileMutation("left4dead2/addons/plugin.smx"); err == nil {
		t.Fatal("non-VPK load-bearing extension was accepted")
	}
}

func TestQQFlashMetadataHasNoFileCountOrSizeLimit(t *testing.T) {
	page := qqSharePage(t, "fileset-large", 1<<40)
	share, err := parseQQFlashSharePage([]byte(page), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if share.TotalFiles != 1<<40 {
		t.Fatalf("total files = %d", share.TotalFiles)
	}
	transport := qqRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"retcode":0,"error":{"code":0},"data":{"file_lists":[{"is_end":true,"pagination_info":"done","parent_id":"","file_list":[{"name":"large.rar","cli_fileid":"large","is_dir":false,"file_physical_size":"21474836480","safe_status":3,"physical":{"id":"physical-large","status":2,"download_limit_status":0}}]}]}}`
		return qqResponse(http.StatusOK, body, nil), nil
	})
	client := &qqFlashClient{metadata: &http.Client{Transport: transport}}
	files, _, _, err := client.listFolder(context.Background(), "fileset-large", "", "", "device")
	if err != nil || len(files) != 1 || files[0].Size != 20<<30 {
		t.Fatalf("large QQ file metadata = %#v, %v", files, err)
	}
	for _, name := range []string{"../escape", "CON.txt", "folder/name", "file:stream"} {
		if err := validateQQFlashEntryName(name); err == nil {
			t.Fatalf("unsafe QQ name accepted: %q", name)
		}
	}
}

func TestQQFlashLiveShareMetadata(t *testing.T) {
	shareURL := os.Getenv("RUN_QQ_FLASH_SHARE")
	if shareURL == "" {
		t.Skip("set RUN_QQ_FLASH_SHARE to validate a live QQ flash share without downloading its files")
	}
	client := newQQFlashClient()
	share, deviceID, err := client.fetchShare(context.Background(), shareURL)
	if err != nil {
		t.Fatal(err)
	}
	files, _, _, err := client.listFolder(context.Background(), share.FilesetID, "", "", deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if share.TotalFiles > 0 && len(files) == 0 {
		t.Fatalf("live share reports %d files but returned an empty root listing", share.TotalFiles)
	}
}

func TestQQFlashLiveImport(t *testing.T) {
	shareURL := os.Getenv("RUN_QQ_FLASH_DOWNLOAD")
	if shareURL == "" {
		t.Skip("set RUN_QQ_FLASH_DOWNLOAD to import a live QQ flash share into a temporary instance")
	}
	result, err := newQQFlashClient().importFiles(context.Background(), t.TempDir(), shareURL, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["files_imported"] == uint64(0) {
		t.Fatalf("live QQ flash import returned no files: %#v", result)
	}
}
