package agent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSafeSegmentedDownloaderUsesEightRangesAndAssemblesFile(t *testing.T) {
	raw := bytes.Repeat([]byte("0123456789abcdef"), 1<<15)
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start, end, ranged := testDownloadRange(t, request, int64(len(raw)))
		if !ranged {
			writer.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			_, _ = writer.Write(raw)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		if request.Header.Get("Range") != "bytes=0-0" {
			if request.Header.Get("If-Range") != `"stable-object"` {
				t.Errorf("segmented request omitted stable If-Range: %q", request.Header.Get("If-Range"))
			}
			time.Sleep(15 * time.Millisecond)
		}
		body := raw[start : end+1]
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("ETag", `"stable-object"`)
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(raw)))
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	root := t.TempDir()
	progress := make([]int, 0, 100)
	downloader := newSafeSegmentedDownloader(newDownloadConcurrencyLimiter(8))
	downloader.segmentSize = 32 << 10
	downloader.retryDelay = time.Millisecond
	result, err := downloader.Download(context.Background(), safeDownloadOptions{
		Client: httptestClient(server), URL: staticDownloadURL(server.URL), Root: root, Relative: "downloads/file.bin",
		ExpectedSize: int64(len(raw)), Mode: 0o600, Replace: false, RejectLink: true, Concurrency: 8,
		Progress: func(value int) { progress = append(progress, value) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Segmented || result.Concurrency != 8 || maximum.Load() < 2 || maximum.Load() > 8 {
		t.Fatalf("segmented result = %#v, maximum concurrency = %d", result, maximum.Load())
	}
	installed, err := os.ReadFile(filepath.Join(root, "downloads", "file.bin"))
	if err != nil || !bytes.Equal(installed, raw) {
		t.Fatalf("assembled file mismatch: size=%d, err=%v", len(installed), err)
	}
	for index := 1; index < len(progress); index++ {
		if progress[index] < progress[index-1] {
			t.Fatalf("download progress moved backwards: %#v", progress)
		}
	}
	if len(progress) == 0 || progress[len(progress)-1] != 100 {
		t.Fatalf("download did not finish progress: %#v", progress)
	}
}

func TestSafeSegmentedDownloaderDefaultsToFourAndCapsAtEight(t *testing.T) {
	if safeDownloadDefaultConcurrency != 4 || safeDownloadMaxConcurrency != 8 || qqFlashDownloadConcurrency != 8 {
		t.Fatalf("download concurrency defaults changed: generic=%d max=%d qq=%d", safeDownloadDefaultConcurrency, safeDownloadMaxConcurrency, qqFlashDownloadConcurrency)
	}
	if got := safeDownloadWorkerCount(1<<30, 1, 0); got != 1 {
		t.Fatalf("explicit worker helper minimum = %d", got)
	}
	if got := safeDownloadWorkerCount(1<<30, 1, safeDownloadDefaultConcurrency); got != 4 {
		t.Fatalf("default worker count = %d", got)
	}
	if got := safeDownloadWorkerCount(1<<30, 1, 100); got != 8 {
		t.Fatalf("worker hard cap = %d", got)
	}
}

func TestDownloadConcurrencyLimiterEnforcesNodeWideHardCap(t *testing.T) {
	limiter := newDownloadConcurrencyLimiter(100)
	releases := make([]func(), 0, safeDownloadMaxConcurrency)
	for index := 0; index < safeDownloadMaxConcurrency; index++ {
		release, err := limiter.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	blockedContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.acquire(blockedContext); err != context.DeadlineExceeded {
		t.Fatalf("ninth node download slot was not blocked: %v", err)
	}
	releases[0]()
	replacement, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	replacement()
	for _, release := range releases[1:] {
		release()
	}
}

func TestSafeSegmentedDownloaderRefreshesExpiredURL(t *testing.T) {
	raw := bytes.Repeat([]byte("refresh"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/expired" {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		start, end, ranged := testDownloadRange(t, request, int64(len(raw)))
		if !ranged {
			writer.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			_, _ = writer.Write(raw)
			return
		}
		body := raw[start : end+1]
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(raw)))
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(body)
	}))
	defer server.Close()
	var calls atomic.Int32
	provider := func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			return server.URL + "/expired", nil
		}
		return server.URL + "/file", nil
	}
	root := t.TempDir()
	downloader := newSafeSegmentedDownloader(newDownloadConcurrencyLimiter(8))
	downloader.segmentSize = 1024
	downloader.retryDelay = time.Millisecond
	result, err := downloader.Download(context.Background(), safeDownloadOptions{
		Client: server.Client(), URL: provider, Root: root, Relative: "file.bin", ExpectedSize: int64(len(raw)),
		Mode: 0o600, Replace: false, Concurrency: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Segmented || calls.Load() != 2 {
		t.Fatalf("expired URL refresh result=%#v provider_calls=%d", result, calls.Load())
	}
}

func TestSafeSegmentedDownloaderFallsBackWhenRangeIsIgnored(t *testing.T) {
	raw := bytes.Repeat([]byte("fallback"), 1<<14)
	var ranged, full atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Range") != "" {
			ranged.Add(1)
		} else {
			full.Add(1)
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = writer.Write(raw)
	}))
	defer server.Close()
	root := t.TempDir()
	downloader := newSafeSegmentedDownloader(newDownloadConcurrencyLimiter(8))
	downloader.segmentSize = 1024
	result, err := downloader.Download(context.Background(), safeDownloadOptions{
		Client: httptestClient(server), URL: staticDownloadURL(server.URL), Root: root, Relative: "file.bin",
		ExpectedSize: int64(len(raw)), Mode: 0o600, Replace: false, Concurrency: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	installed, readErr := os.ReadFile(filepath.Join(root, "file.bin"))
	if readErr != nil || !bytes.Equal(installed, raw) || result.Segmented || result.Concurrency != 1 || ranged.Load() == 0 || full.Load() == 0 {
		t.Fatalf("fallback result=%#v ranged=%d full=%d size=%d err=%v", result, ranged.Load(), full.Load(), len(installed), readErr)
	}
}

func TestSafeSegmentedDownloaderRejectsInvalidContentRange(t *testing.T) {
	raw := bytes.Repeat([]byte("range"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 1-1/%d", len(raw)))
		writer.Header().Set("Content-Length", "1")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(raw[:1])
	}))
	defer server.Close()
	root := t.TempDir()
	downloader := newSafeSegmentedDownloader(newDownloadConcurrencyLimiter(8))
	downloader.segmentSize = 1024
	_, err := downloader.Download(context.Background(), safeDownloadOptions{
		Client: httptestClient(server), URL: staticDownloadURL(server.URL), Root: root, Relative: "file.bin",
		ExpectedSize: int64(len(raw)), Mode: 0o600, Replace: false, Concurrency: 8,
	})
	if err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("invalid Content-Range was accepted: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "file.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid ranged download committed a target: %v", statErr)
	}
}

func TestSafeSegmentedDownloaderCancellationRemovesTemporaryFile(t *testing.T) {
	raw := bytes.Repeat([]byte("cancel"), 4096)
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start, end, _ := testDownloadRange(t, request, int64(len(raw)))
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(raw)))
		writer.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		writer.WriteHeader(http.StatusPartialContent)
		if request.Header.Get("Range") == "bytes=0-0" {
			_, _ = writer.Write(raw[:1])
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	root := t.TempDir()
	downloader := newSafeSegmentedDownloader(newDownloadConcurrencyLimiter(8))
	downloader.segmentSize = 1024
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := downloader.Download(ctx, safeDownloadOptions{
			Client: httptestClient(server), URL: staticDownloadURL(server.URL), Root: root, Relative: "nested/file.bin",
			ExpectedSize: int64(len(raw)), Mode: 0o600, Replace: false, Concurrency: 8,
		})
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("segmented requests did not start")
	}
	if err := <-done; !errorsIsContext(err) {
		t.Fatalf("canceled download returned %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled download left temporary entries: %#v", entries)
	}
}

func TestManagedDownloadFileRejectsLinkedAncestor(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	file, err := createManagedDownloadFile(root, "linked/file.bin", secureWriteOptions{Mode: 0o600, Replace: false, RejectExistingLink: true})
	if file != nil {
		file.Abort()
	}
	if err == nil {
		t.Fatal("managed download followed a linked ancestor")
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "file.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("managed download wrote outside its root: %v", statErr)
	}
}

func testDownloadRange(t *testing.T, request *http.Request, size int64) (int64, int64, bool) {
	t.Helper()
	value := request.Header.Get("Range")
	if value == "" {
		return 0, size - 1, false
	}
	var start, end int64
	if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= size {
		t.Fatalf("invalid test byte range %q", value)
	}
	return start, end, true
}

func staticDownloadURL(value string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return value, nil }
}

func httptestClient(server *httptest.Server) *http.Client { return server.Client() }

func errorsIsContext(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
