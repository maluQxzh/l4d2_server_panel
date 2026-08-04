package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- QQ's public web protocol requires HMAC-SHA1.
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
	"time"
)

const (
	maxQQFlashPageSize = int64(4 << 20)
	maxQQFlashRPCSize  = int64(16 << 20)
	qqFlashRPCBase     = "https://qfile.qq.com/http2rpc/gotrpc/noauth/"
	qqFlashListRPC     = "trpc.file.FileFlashTrans/GetFileList"
	qqFlashDownloadRPC = "trpc.qqntv2.richmedia.InnerProxy/BatchDownload"
	qqFlashHMACKey     = "9EB18BB9ED457684"
	qqFlashUserAgent   = "l4d2panel-agent/1"
)

type qqFlashShare struct {
	FilesetID  string
	TotalFiles int64
}

type qqFlashFile struct {
	Name       string
	FileID     string
	PhysicalID string
	Size       int64
	IsDir      bool
}

type qqFlashClient struct {
	metadata *http.Client
	download *http.Client
}

func newQQFlashClient() *qqFlashClient {
	return &qqFlashClient{
		metadata: newRestrictedHTTPClient(map[string]bool{"qfile.qq.com": true}, 30*time.Second, 0),
		download: newRestrictedHTTPClient(map[string]bool{"multimedia.qfile.qq.com": true}, 0, 0),
	}
}

func importQQFlashFile(ctx context.Context, instancePath, relative, shareURL string, progress ProgressFunc) (map[string]any, error) {
	return newQQFlashClient().importFiles(ctx, instancePath, relative, shareURL, progress)
}

func (c *qqFlashClient) importFiles(ctx context.Context, instancePath, relative, shareURL string, progress ProgressFunc) (map[string]any, error) {
	destination, err := cleanManagedRelative(relative)
	if err != nil || destination == "" {
		return nil, errors.New("a managed destination directory is required")
	}
	if err := validateManagedFileMutation(destination); err != nil {
		return nil, err
	}
	if err := validateQQFlashShareURL(shareURL); err != nil {
		return nil, err
	}
	progress(5, "reading QQ flash share metadata")
	share, deviceID, err := c.fetchShare(ctx, shareURL)
	if err != nil {
		return nil, err
	}
	type folder struct {
		ID       string
		Relative string
	}
	folders := []folder{{}}
	visitedFolders := map[string]struct{}{"": {}}
	seenTargets := make(map[string]struct{})
	var imported uint64
	for folderIndex := 0; folderIndex < len(folders); folderIndex++ {
		current := folders[folderIndex]
		pagination := ""
		seenPages := make(map[string]struct{})
		for {
			files, next, done, listErr := c.listFolder(ctx, share.FilesetID, current.ID, pagination, deviceID)
			if listErr != nil {
				return nil, qqFlashPartialError(imported, listErr)
			}
			for _, file := range files {
				if err := validateQQFlashEntryName(file.Name); err != nil {
					return nil, qqFlashPartialError(imported, err)
				}
				child := pathpkg.Join(current.Relative, file.Name)
				if file.IsDir {
					if file.FileID == "" || len(file.FileID) > 1024 {
						return nil, qqFlashPartialError(imported, errors.New("QQ flash folder identifier is invalid"))
					}
					if _, duplicate := visitedFolders[file.FileID]; duplicate {
						return nil, qqFlashPartialError(imported, errors.New("QQ flash folder graph contains a duplicate or cycle"))
					}
					visitedFolders[file.FileID] = struct{}{}
					folders = append(folders, folder{ID: file.FileID, Relative: child})
					continue
				}
				target := pathpkg.Join(destination, child)
				cleanedTarget, cleanErr := cleanManagedRelative(target)
				if cleanErr != nil || cleanedTarget != target {
					return nil, qqFlashPartialError(imported, errors.New("QQ flash file path escapes the managed destination"))
				}
				if err := validateManagedFileMutation(cleanedTarget); err != nil {
					return nil, qqFlashPartialError(imported, err)
				}
				caseFolded := strings.ToLower(cleanedTarget)
				if _, duplicate := seenTargets[caseFolded]; duplicate {
					return nil, qqFlashPartialError(imported, errors.New("QQ flash share contains colliding file paths"))
				}
				seenTargets[caseFolded] = struct{}{}
				percent := 10
				if share.TotalFiles > 0 && imported < uint64(share.TotalFiles) {
					percent += int(imported * 85 / uint64(share.TotalFiles))
				}
				progress(percent, "downloading QQ flash files")
				if err := c.downloadFile(ctx, instancePath, cleanedTarget, file, deviceID); err != nil {
					return nil, qqFlashPartialError(imported, err)
				}
				imported++
			}
			if done {
				break
			}
			if next == "" {
				return nil, qqFlashPartialError(imported, errors.New("QQ flash pagination ended without a cursor"))
			}
			if _, repeated := seenPages[next]; repeated {
				return nil, qqFlashPartialError(imported, errors.New("QQ flash pagination cursor repeated"))
			}
			seenPages[next] = struct{}{}
			pagination = next
		}
	}
	progress(100, "QQ flash files imported")
	return map[string]any{"path": destination, "files_imported": imported, "source": "qq_flash"}, nil
}

func qqFlashPartialError(imported uint64, err error) error {
	if imported == 0 {
		return err
	}
	return fmt.Errorf("QQ flash import stopped after %d files; completed files remain committed: %w", imported, err)
}

func validateQQFlashEntryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.TrimSpace(name) != name || strings.ContainsAny(name, `/\:`) {
		return errors.New("QQ flash share contains an unsafe file name")
	}
	for _, char := range name {
		if char == 0 || char < 0x20 || char == 0x7f {
			return errors.New("QQ flash share contains an unsafe file name")
		}
	}
	trimmed := strings.TrimRight(name, ". ")
	if trimmed != name {
		return errors.New("QQ flash share contains a file name with trailing dots or spaces")
	}
	base := strings.ToUpper(strings.SplitN(trimmed, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return errors.New("QQ flash share contains a reserved device name")
	}
	return nil
}

func validateQQFlashShareURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "qfile.qq.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("QQ flash source must be an https://qfile.qq.com/q/... share link")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 2 || parts[0] != "q" || len(parts[1]) < 6 || len(parts[1]) > 64 {
		return errors.New("QQ flash source must be an https://qfile.qq.com/q/... share link")
	}
	for _, char := range parts[1] {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return errors.New("QQ flash share code is invalid")
		}
	}
	return nil
}

func (c *qqFlashClient) fetchShare(ctx context.Context, shareURL string) (qqFlashShare, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, shareURL, nil)
	if err != nil {
		return qqFlashShare{}, "", err
	}
	request.Header.Set("User-Agent", qqFlashUserAgent)
	response, err := c.metadata.Do(request)
	if err != nil {
		return qqFlashShare{}, "", errors.New("read QQ flash share page failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return qqFlashShare{}, "", fmt.Errorf("QQ flash share page returned %s", response.Status)
	}
	if response.ContentLength > maxQQFlashPageSize {
		return qqFlashShare{}, "", errors.New("QQ flash share metadata is too large")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxQQFlashPageSize+1))
	if err != nil || int64(len(raw)) > maxQQFlashPageSize {
		return qqFlashShare{}, "", errors.New("read QQ flash share metadata failed")
	}
	deviceID := ""
	for _, cookie := range response.Cookies() {
		if cookie.Name == "deviceId" {
			deviceID = cookie.Value
			break
		}
	}
	if deviceID == "" || len(deviceID) > 256 {
		return qqFlashShare{}, "", errors.New("QQ flash share page did not provide a device identifier")
	}
	share, err := parseQQFlashSharePage(raw, time.Now())
	return share, deviceID, err
}

func parseQQFlashSharePage(html []byte, now time.Time) (qqFlashShare, error) {
	marker := []byte(`id="__NUXT_DATA__"`)
	start := bytes.Index(html, marker)
	if start < 0 {
		return qqFlashShare{}, errors.New("QQ flash share metadata was not found")
	}
	start = bytes.IndexByte(html[start+len(marker):], '>') + start + len(marker)
	if start < len(marker) {
		return qqFlashShare{}, errors.New("QQ flash share metadata is malformed")
	}
	start++
	endOffset := bytes.Index(html[start:], []byte("</script>"))
	if endOffset < 0 {
		return qqFlashShare{}, errors.New("QQ flash share metadata is incomplete")
	}
	decoder := json.NewDecoder(bytes.NewReader(html[start : start+endOffset]))
	decoder.UseNumber()
	var table []any
	if err := decoder.Decode(&table); err != nil || len(table) == 0 {
		return qqFlashShare{}, errors.New("QQ flash share metadata is invalid")
	}
	rootValue, err := newNuxtResolver(table).resolveIndex(0)
	if err != nil {
		return qqFlashShare{}, errors.New("QQ flash share metadata references are invalid")
	}
	root, ok := rootValue.(map[string]any)
	if !ok {
		return qqFlashShare{}, errors.New("QQ flash share metadata root is invalid")
	}
	data, ok := qqMap(root["data"])
	if !ok {
		return qqFlashShare{}, errors.New("QQ flash share data is missing")
	}
	var filesetResponse map[string]any
	for key, value := range data {
		if strings.HasPrefix(key, "trpc.file.FileFlashTrans/GetFileset_") {
			filesetResponse, _ = qqMap(value)
			break
		}
	}
	if !qqRPCSucceeded(filesetResponse) {
		return qqFlashShare{}, errors.New("QQ flash share is unavailable or expired")
	}
	filesetData, _ := qqMap(filesetResponse["data"])
	fileset, ok := qqMap(filesetData["fileset"])
	if !ok {
		return qqFlashShare{}, errors.New("QQ flash fileset metadata is missing")
	}
	if expired, ok := qqInt64(fileset["expired_time"]); ok && expired > 0 && now.Unix() >= expired {
		return qqFlashShare{}, errors.New("QQ flash share has expired")
	}
	filesetID, _ := fileset["fileset_id"].(string)
	if filesetID == "" || len(filesetID) > 128 {
		return qqFlashShare{}, errors.New("QQ flash fileset identifier is invalid")
	}
	totalFiles, _ := qqInt64(fileset["total_file_count"])
	return qqFlashShare{FilesetID: filesetID, TotalFiles: totalFiles}, nil
}

func qqRPCSucceeded(response map[string]any) bool {
	if response == nil {
		return false
	}
	retcode, ok := qqInt64(response["retcode"])
	return ok && retcode == 0
}

func qqMap(value any) (map[string]any, bool) {
	item, ok := value.(map[string]any)
	return item, ok
}

func qqInt64(value any) (int64, bool) {
	switch item := value.(type) {
	case int64:
		return item, true
	case int:
		return int64(item), true
	case float64:
		return int64(item), item == float64(int64(item))
	case json.Number:
		parsed, err := item.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(item, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

type nuxtResolver struct {
	table     []any
	cache     map[int]any
	resolving map[int]bool
}

func newNuxtResolver(table []any) *nuxtResolver {
	return &nuxtResolver{table: table, cache: make(map[int]any), resolving: make(map[int]bool)}
}

func (r *nuxtResolver) resolveIndex(index int) (any, error) {
	if index < 0 || index >= len(r.table) {
		return nil, errors.New("Nuxt reference is out of range")
	}
	if value, ok := r.cache[index]; ok {
		return value, nil
	}
	if r.resolving[index] {
		return nil, errors.New("cyclic Nuxt metadata is unsupported")
	}
	r.resolving[index] = true
	defer delete(r.resolving, index)
	raw := r.table[index]
	switch value := raw.(type) {
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			r.cache[index] = integer
			return integer, nil
		}
		decimal, err := value.Float64()
		if err != nil {
			return nil, err
		}
		r.cache[index] = decimal
		return decimal, nil
	case map[string]any:
		result := make(map[string]any, len(value))
		r.cache[index] = result
		for key, child := range value {
			resolved, err := r.resolveReference(child)
			if err != nil {
				return nil, err
			}
			result[key] = resolved
		}
		return result, nil
	case []any:
		if len(value) >= 2 {
			if tag, ok := value[0].(string); ok && (tag == "ShallowReactive" || tag == "Reactive" || tag == "Ref" || tag == "ShallowRef" || tag == "Readonly" || tag == "ShallowReadonly") {
				resolved, err := r.resolveReference(value[1])
				if err == nil {
					r.cache[index] = resolved
				}
				return resolved, err
			}
		}
		result := make([]any, len(value))
		r.cache[index] = result
		for childIndex, child := range value {
			resolved, err := r.resolveReference(child)
			if err != nil {
				return nil, err
			}
			result[childIndex] = resolved
		}
		return result, nil
	default:
		r.cache[index] = value
		return value, nil
	}
}

func (r *nuxtResolver) resolveReference(value any) (any, error) {
	if reference, ok := value.(json.Number); ok {
		index, err := reference.Int64()
		if err != nil || index >= int64(len(r.table)) {
			return nil, errors.New("invalid Nuxt reference")
		}
		if index < 0 {
			return nil, nil
		}
		return r.resolveIndex(int(index))
	}
	return value, nil
}

type qqFileListRequest struct {
	FilesetID           string              `json:"fileset_id"`
	RequestInfos        []qqFileListReqInfo `json:"req_infos"`
	SupportFolderStatus bool                `json:"support_folder_status"`
	SceneType           int                 `json:"scene_type"`
}

type qqFileListReqInfo struct {
	ParentID        string            `json:"parent_id"`
	RequestDepth    int               `json:"req_depth"`
	Count           int               `json:"count"`
	SortConditions  []qqSortCondition `json:"sort_conditions"`
	FilterCondition qqFilterCondition `json:"filter_condition"`
	PaginationInfo  string            `json:"pagination_info,omitempty"`
}

type qqSortCondition struct {
	SortField int `json:"sort_field"`
	SortOrder int `json:"sort_order"`
}

type qqFilterCondition struct {
	FileCategory   int  `json:"file_category"`
	ExcludeDeleted bool `json:"exclude_deleted"`
}

func (c *qqFlashClient) listFolder(ctx context.Context, filesetID, parentID, pagination, deviceID string) ([]qqFlashFile, string, bool, error) {
	payload := qqFileListRequest{FilesetID: filesetID, SupportFolderStatus: true, SceneType: 103,
		RequestInfos: []qqFileListReqInfo{{ParentID: parentID, RequestDepth: 1, Count: 200,
			SortConditions:  []qqSortCondition{{SortField: 4, SortOrder: 2}, {SortField: 0, SortOrder: 1}},
			FilterCondition: qqFilterCondition{FileCategory: 0, ExcludeDeleted: true}, PaginationInfo: pagination}}}
	var response struct {
		Retcode int `json:"retcode"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
		Data struct {
			Lists []struct {
				IsEnd          bool   `json:"is_end"`
				PaginationInfo string `json:"pagination_info"`
				ParentID       string `json:"parent_id"`
				Files          []struct {
					Name         string `json:"name"`
					ClientFileID string `json:"cli_fileid"`
					IsDir        bool   `json:"is_dir"`
					FileSize     string `json:"file_physical_size"`
					Physical     struct {
						ID                  string `json:"id"`
						Status              int    `json:"status"`
						DownloadLimitStatus int    `json:"download_limit_status"`
					} `json:"physical"`
				} `json:"file_list"`
			} `json:"file_lists"`
		} `json:"data"`
	}
	if err := c.signedRPC(ctx, qqFlashListRPC, "0x93d4", "1", payload, deviceID, &response); err != nil {
		return nil, "", false, err
	}
	if response.Retcode != 0 || response.Error.Code != 0 || len(response.Data.Lists) != 1 || response.Data.Lists[0].ParentID != parentID {
		return nil, "", false, errors.New("QQ flash returned an invalid file list")
	}
	list := response.Data.Lists[0]
	files := make([]qqFlashFile, 0, len(list.Files))
	for _, item := range list.Files {
		file := qqFlashFile{Name: item.Name, FileID: item.ClientFileID, IsDir: item.IsDir}
		if !item.IsDir {
			size, err := strconv.ParseInt(item.FileSize, 10, 64)
			if err != nil || size < 0 {
				return nil, "", false, errors.New("QQ flash file size is invalid")
			}
			if item.Physical.Status != 2 || item.Physical.DownloadLimitStatus != 0 || item.Physical.ID == "" || len(item.Physical.ID) > 1024 {
				return nil, "", false, errors.New("QQ flash file is not ready for anonymous download")
			}
			file.PhysicalID, file.Size = item.Physical.ID, size
		}
		files = append(files, file)
	}
	return files, list.PaginationInfo, list.IsEnd, nil
}

type qqBatchDownloadRequest struct {
	RequestHead  qqBatchRequestHead    `json:"req_head"`
	DownloadInfo []qqBatchDownloadInfo `json:"download_info"`
	SceneType    int                   `json:"scene_type"`
}

type qqBatchRequestHead struct {
	Agent int `json:"agent"`
}

type qqBatchDownloadInfo struct {
	BatchID       string          `json:"batch_id"`
	Scene         qqDownloadScene `json:"scene"`
	IndexNode     qqIndexNode     `json:"index_node"`
	URLType       int             `json:"url_type"`
	DownloadScene int             `json:"download_scene"`
}

type qqDownloadScene struct {
	BusinessType int `json:"business_type"`
	AppType      int `json:"app_type"`
	SceneType    int `json:"scene_type"`
}

type qqIndexNode struct {
	FileUUID string `json:"file_uuid"`
}

func (c *qqFlashClient) fetchDownloadURL(ctx context.Context, physicalID, deviceID string) (string, error) {
	payload := qqBatchDownloadRequest{RequestHead: qqBatchRequestHead{Agent: 8}, SceneType: 103,
		DownloadInfo: []qqBatchDownloadInfo{{BatchID: physicalID, Scene: qqDownloadScene{BusinessType: 4, AppType: 22, SceneType: 5},
			IndexNode: qqIndexNode{FileUUID: physicalID}, URLType: 2, DownloadScene: 0}}}
	var response struct {
		Retcode int `json:"retcode"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
		Data struct {
			Downloads []struct {
				URL     string `json:"url"`
				RetCode string `json:"ret_code"`
			} `json:"download_rsp"`
		} `json:"data"`
	}
	if err := c.signedRPC(ctx, qqFlashDownloadRPC, "0x9248", "4", payload, deviceID, &response); err != nil {
		return "", err
	}
	if response.Retcode != 0 || response.Error.Code != 0 || len(response.Data.Downloads) != 1 || response.Data.Downloads[0].RetCode != "0" {
		return "", errors.New("QQ flash did not provide a download URL")
	}
	downloadURL := response.Data.Downloads[0].URL
	parsed, err := url.Parse(downloadURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "multimedia.qfile.qq.com" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("QQ flash returned an untrusted download URL")
	}
	return downloadURL, nil
}

func (c *qqFlashClient) downloadFile(ctx context.Context, instancePath, relative string, file qqFlashFile, deviceID string) error {
	downloadURL, err := c.fetchDownloadURL(ctx, file.PhysicalID, deviceID)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return errors.New("create QQ flash download request")
	}
	request.Header.Set("User-Agent", qqFlashUserAgent)
	request.Header.Set("Accept-Encoding", "identity")
	response, err := c.download.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("download QQ flash file failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("QQ flash file server returned %s", response.Status)
	}
	if response.ContentLength >= 0 && response.ContentLength != file.Size {
		return errors.New("QQ flash file size changed before download")
	}
	options := secureWriteOptions{Mode: 0o640, ExpectedSize: file.Size, Replace: true, RejectExistingLink: true}
	_, err = writeFileWithinNoFollow(instancePath, relative, response.Body, options)
	return err
}

func (c *qqFlashClient) signedRPC(ctx context.Context, method, command, service string, payload any, deviceID string, destination any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	nonce, err := qqFlashNonce()
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha1.New, []byte(qqFlashHMACKey))
	_, _ = mac.Write(body)
	_, _ = mac.Write([]byte(nonce))
	_, _ = mac.Write([]byte(timestamp))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, qqFlashRPCBase+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", "uin=9000002; p_uin=9000002; deviceId="+deviceID)
	request.Header.Set("User-Agent", qqFlashUserAgent)
	request.Header.Set("x-device-id", deviceID)
	request.Header.Set("x-oidb", fmt.Sprintf(`{"uint32_command":"%s", "uint32_service_type":"%s"}`, command, service))
	request.Header.Set("x-qq-ar-nonce", nonce)
	request.Header.Set("x-qq-ar-timestamp", timestamp)
	request.Header.Set("x-qq-ar-signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := c.metadata.Do(request)
	if err != nil {
		return errors.New("request QQ flash metadata failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("QQ flash metadata server returned %s", response.Status)
	}
	if response.ContentLength > maxQQFlashRPCSize {
		return errors.New("QQ flash metadata response is too large")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxQQFlashRPCSize+1))
	if err != nil || int64(len(raw)) > maxQQFlashRPCSize || json.Unmarshal(raw, destination) != nil {
		return errors.New("QQ flash metadata response is invalid or too large")
	}
	return nil
}

func qqFlashNonce() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return strconv.FormatUint(binary.LittleEndian.Uint64(raw[:])%10000+1, 10), nil
}
