package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	safeDownloadDefaultConcurrency = 4
	safeDownloadMaxConcurrency     = 8
	safeDownloadTargetSegmentSize  = int64(64 << 20)
	safeDownloadBufferSize         = 256 << 10
	safeDownloadMaxAttempts        = 3
)

var (
	errDownloadRangeUnsupported            = errors.New("download server did not honor a byte range")
	errManagedSegmentedDownloadUnsupported = errors.New("secure segmented download writes are unsupported on this platform")
	contentRangePattern                    = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)
)

type downloadConcurrencyLimiter struct {
	tokens chan struct{}
}

func newDownloadConcurrencyLimiter(maximum int) *downloadConcurrencyLimiter {
	maximum = min(max(maximum, 1), safeDownloadMaxConcurrency)
	return &downloadConcurrencyLimiter{tokens: make(chan struct{}, maximum)}
}

func (limiter *downloadConcurrencyLimiter) acquire(ctx context.Context) (func(), error) {
	if limiter == nil {
		return func() {}, nil
	}
	select {
	case limiter.tokens <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-limiter.tokens }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type safeDownloadOptions struct {
	Client  *http.Client
	URL     func(context.Context) (string, error)
	Headers http.Header
	// SourceIsImmutable is reserved for fixed Agent integrations whose remote
	// object identifier cannot be rebound to different bytes. It allows those
	// sources to use byte ranges when an origin exposes no usable HTTP validator.
	SourceIsImmutable bool
	Root              string
	Relative          string
	ExpectedSize      int64
	ExpectedSHA256    string
	Mode              os.FileMode
	Replace           bool
	RejectLink        bool
	Concurrency       int
	Progress          func(int)
}

type safeDownloadResult struct {
	Segmented   bool
	Concurrency int
}

type safeSegmentedDownloader struct {
	limiter     *downloadConcurrencyLimiter
	segmentSize int64
	maxAttempts int
	retryDelay  time.Duration
}

func newSafeSegmentedDownloader(limiter *downloadConcurrencyLimiter) *safeSegmentedDownloader {
	if limiter == nil {
		limiter = newDownloadConcurrencyLimiter(safeDownloadMaxConcurrency)
	}
	return &safeSegmentedDownloader{
		limiter: limiter, segmentSize: safeDownloadTargetSegmentSize,
		maxAttempts: safeDownloadMaxAttempts, retryDelay: 200 * time.Millisecond,
	}
}

func (downloader *safeSegmentedDownloader) Download(ctx context.Context, options safeDownloadOptions) (safeDownloadResult, error) {
	if options.Client == nil || options.URL == nil || options.Root == "" || options.Relative == "" {
		return safeDownloadResult{}, errors.New("safe download destination and HTTP source are required")
	}
	if options.ExpectedSize < 0 {
		return safeDownloadResult{}, errors.New("safe download size cannot be negative")
	}
	if options.Mode == 0 {
		options.Mode = 0o640
	}
	concurrency := options.Concurrency
	if concurrency == 0 {
		concurrency = safeDownloadDefaultConcurrency
	}
	concurrency = min(max(concurrency, 1), safeDownloadMaxConcurrency)
	segmentSize := downloader.segmentSize
	if segmentSize <= 0 {
		segmentSize = safeDownloadTargetSegmentSize
	}
	workers := safeDownloadWorkerCount(options.ExpectedSize, segmentSize, concurrency)
	urls := &safeDownloadURLState{provider: options.URL}
	if _, err := urls.currentURL(ctx); err != nil {
		return safeDownloadResult{}, err
	}
	progress := &safeDownloadProgress{total: options.ExpectedSize, report: options.Progress}
	if workers < 2 {
		err := downloader.downloadSingle(ctx, options, urls, progress)
		return safeDownloadResult{Concurrency: 1}, err
	}
	identity, supported, err := downloader.probeRange(ctx, options, urls)
	if err != nil {
		return safeDownloadResult{}, err
	}
	if !supported {
		err := downloader.downloadSingle(ctx, options, urls, progress)
		return safeDownloadResult{Concurrency: 1}, err
	}

	file, err := createManagedDownloadFile(options.Root, options.Relative, secureWriteOptions{
		Mode: options.Mode, Replace: options.Replace, RejectExistingLink: options.RejectLink,
	})
	if err != nil {
		if errors.Is(err, errManagedSegmentedDownloadUnsupported) {
			err := downloader.downloadSingle(ctx, options, urls, progress)
			return safeDownloadResult{Concurrency: 1}, err
		}
		return safeDownloadResult{}, err
	}
	defer file.Abort()
	if err := file.Truncate(options.ExpectedSize); err != nil {
		return safeDownloadResult{}, fmt.Errorf("preallocate managed download file: %w", err)
	}

	rangeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ranges := partitionSafeDownload(options.ExpectedSize, workers)
	errorsByRange := make([]error, len(ranges))
	var wait sync.WaitGroup
	for index, byteRange := range ranges {
		wait.Add(1)
		go func(index int, byteRange safeDownloadRange) {
			defer wait.Done()
			errorsByRange[index] = downloader.downloadRange(rangeCtx, options, urls, identity, file, byteRange, progress)
			if errorsByRange[index] != nil {
				cancel()
			}
		}(index, byteRange)
	}
	wait.Wait()
	var fatalErr error
	unsupported := false
	for _, rangeErr := range errorsByRange {
		if errors.Is(rangeErr, errDownloadRangeUnsupported) {
			unsupported = true
			continue
		}
		if rangeErr != nil && !errors.Is(rangeErr, context.Canceled) && fatalErr == nil {
			fatalErr = rangeErr
		}
	}
	if fatalErr != nil {
		return safeDownloadResult{}, fatalErr
	}
	if unsupported {
		file.Abort()
		progress.reset()
		err := downloader.downloadSingle(ctx, options, urls, progress)
		return safeDownloadResult{Concurrency: 1}, err
	}
	if err := ctx.Err(); err != nil {
		return safeDownloadResult{}, err
	}
	if size, err := file.Size(); err != nil || size != options.ExpectedSize {
		return safeDownloadResult{}, errors.New("segmented download did not produce the declared file size")
	}
	if err := verifyManagedDownloadSHA(file, options.ExpectedSize, options.ExpectedSHA256); err != nil {
		return safeDownloadResult{}, err
	}
	if err := file.Commit(); err != nil {
		return safeDownloadResult{}, err
	}
	progress.complete()
	return safeDownloadResult{Segmented: true, Concurrency: workers}, nil
}

func safeDownloadWorkerCount(size, segmentSize int64, maximum int) int {
	if size <= 0 || segmentSize <= 0 {
		return 1
	}
	limit := min(max(maximum, 1), safeDownloadMaxConcurrency)
	workers := (size-1)/segmentSize + 1
	if workers >= int64(limit) {
		return limit
	}
	return max(int(workers), 1)
}

type safeDownloadRange struct {
	start int64
	end   int64
}

func partitionSafeDownload(size int64, workers int) []safeDownloadRange {
	workers = min(max(workers, 1), safeDownloadMaxConcurrency)
	ranges := make([]safeDownloadRange, 0, workers)
	base, remainder, next := size/int64(workers), size%int64(workers), int64(0)
	for index := 0; index < workers; index++ {
		length := base
		if int64(index) < remainder {
			length++
		}
		ranges = append(ranges, safeDownloadRange{start: next, end: next + length - 1})
		next += length
	}
	return ranges
}

type safeDownloadIdentity struct {
	etag         string
	lastModified string
}

func (identity safeDownloadIdentity) apply(request *http.Request) {
	if identity.etag != "" && !strings.HasPrefix(strings.TrimSpace(identity.etag), "W/") {
		request.Header.Set("If-Range", identity.etag)
	} else if identity.lastModified != "" {
		request.Header.Set("If-Range", identity.lastModified)
	}
}

func (identity safeDownloadIdentity) validate(response *http.Response) error {
	if identity.etag != "" && strings.TrimSpace(response.Header.Get("ETag")) != identity.etag {
		return errors.New("download ETag changed between byte ranges")
	}
	if identity.lastModified != "" && strings.TrimSpace(response.Header.Get("Last-Modified")) != identity.lastModified {
		return errors.New("download Last-Modified changed between byte ranges")
	}
	return nil
}

func safeDownloadIdentityFromResponse(response *http.Response, sourceIsImmutable bool) safeDownloadIdentity {
	// A strong ETag is the most precise HTTP representation validator. Do not
	// additionally require Last-Modified when one is available: intermediaries
	// can legally omit or normalize the weaker timestamp independently.
	etag := strings.TrimSpace(response.Header.Get("ETag"))
	if etag != "" && !strings.HasPrefix(strings.ToUpper(etag), "W/") {
		return safeDownloadIdentity{etag: etag}
	}
	if sourceIsImmutable {
		return safeDownloadIdentity{}
	}
	return safeDownloadIdentity{lastModified: strings.TrimSpace(response.Header.Get("Last-Modified"))}
}

func (downloader *safeSegmentedDownloader) probeRange(ctx context.Context, options safeDownloadOptions, urls *safeDownloadURLState) (safeDownloadIdentity, bool, error) {
	for attempt := 0; attempt < downloader.attempts(); attempt++ {
		downloadURL, err := urls.currentURL(ctx)
		if err != nil {
			return safeDownloadIdentity{}, false, err
		}
		request, err := safeDownloadRequest(ctx, downloadURL, options.Headers)
		if err != nil {
			return safeDownloadIdentity{}, false, err
		}
		request.Header.Set("Range", "bytes=0-0")
		response, err := downloader.do(request, options.Client)
		if err != nil {
			if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
				return safeDownloadIdentity{}, false, waitErr
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			_ = response.Body.Close()
			return safeDownloadIdentity{}, false, nil
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			_ = response.Body.Close()
			if _, err := urls.refresh(ctx, downloadURL); err != nil {
				return safeDownloadIdentity{}, false, err
			}
			continue
		}
		if transientDownloadStatus(response.StatusCode) {
			_ = response.Body.Close()
			if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
				return safeDownloadIdentity{}, false, waitErr
			}
			continue
		}
		if response.StatusCode != http.StatusPartialContent {
			status := response.StatusCode
			_ = response.Body.Close()
			return safeDownloadIdentity{}, false, fmt.Errorf("download range probe returned HTTP %d", status)
		}
		if err := validateSafeDownloadRangeResponse(response, 0, 0, options.ExpectedSize); err != nil {
			_ = response.Body.Close()
			return safeDownloadIdentity{}, false, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 2))
		_ = response.Body.Close()
		if readErr != nil || len(raw) != 1 {
			return safeDownloadIdentity{}, false, errors.New("download range probe returned an invalid body")
		}
		return safeDownloadIdentityFromResponse(response, options.SourceIsImmutable), true, nil
	}
	return safeDownloadIdentity{}, false, errors.New("download range probe exhausted retries")
}

func (downloader *safeSegmentedDownloader) downloadRange(ctx context.Context, options safeDownloadOptions, urls *safeDownloadURLState,
	identity safeDownloadIdentity, file *managedDownloadFile, byteRange safeDownloadRange, progress *safeDownloadProgress) error {
	next := byteRange.start
	for attempt := 0; attempt < downloader.attempts() && next <= byteRange.end; attempt++ {
		downloadURL, err := urls.currentURL(ctx)
		if err != nil {
			return err
		}
		request, err := safeDownloadRequest(ctx, downloadURL, options.Headers)
		if err != nil {
			return err
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", next, byteRange.end))
		identity.apply(request)
		response, err := downloader.do(request, options.Client)
		if err != nil {
			if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
				return waitErr
			}
			continue
		}
		if response.StatusCode == http.StatusOK {
			_ = response.Body.Close()
			return errDownloadRangeUnsupported
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			_ = response.Body.Close()
			if _, err := urls.refresh(ctx, downloadURL); err != nil {
				return err
			}
			continue
		}
		if transientDownloadStatus(response.StatusCode) {
			_ = response.Body.Close()
			if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
				return waitErr
			}
			continue
		}
		if response.StatusCode != http.StatusPartialContent {
			status := response.StatusCode
			_ = response.Body.Close()
			return fmt.Errorf("download byte range returned HTTP %d", status)
		}
		if err := validateSafeDownloadRangeResponse(response, next, byteRange.end, options.ExpectedSize); err != nil {
			_ = response.Body.Close()
			return err
		}
		if err := identity.validate(response); err != nil {
			_ = response.Body.Close()
			return err
		}
		written, readErr := writeSafeDownloadRange(file, response.Body, next, byteRange.end, progress)
		_ = response.Body.Close()
		next += written
		if next > byteRange.end {
			return nil
		}
		if readErr == nil {
			readErr = io.ErrUnexpectedEOF
		}
		if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
			return waitErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("download byte range exhausted retries")
}

func writeSafeDownloadRange(file *managedDownloadFile, body io.Reader, start, end int64, progress *safeDownloadProgress) (int64, error) {
	offset, remaining := start, end-start+1
	buffer := make([]byte, safeDownloadBufferSize)
	var written int64
	for remaining > 0 {
		readSize := min(int64(len(buffer)), remaining)
		count, readErr := body.Read(buffer[:readSize])
		if count > 0 {
			fileCount, writeErr := file.WriteAt(buffer[:count], offset)
			if writeErr != nil || fileCount != count {
				if writeErr != nil {
					return written, writeErr
				}
				return written, io.ErrShortWrite
			}
			offset += int64(count)
			remaining -= int64(count)
			written += int64(count)
			progress.add(int64(count))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && remaining == 0 {
				return written, nil
			}
			return written, readErr
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

func (downloader *safeSegmentedDownloader) downloadSingle(ctx context.Context, options safeDownloadOptions, urls *safeDownloadURLState, progress *safeDownloadProgress) error {
	progress.reset()
	for attempt := 0; attempt < downloader.attempts(); attempt++ {
		downloadURL, err := urls.currentURL(ctx)
		if err != nil {
			return err
		}
		request, err := safeDownloadRequest(ctx, downloadURL, options.Headers)
		if err != nil {
			return err
		}
		response, err := downloader.do(request, options.Client)
		if err != nil {
			if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
				return waitErr
			}
			continue
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			_ = response.Body.Close()
			if _, err := urls.refresh(ctx, downloadURL); err != nil {
				return err
			}
			continue
		}
		if transientDownloadStatus(response.StatusCode) {
			_ = response.Body.Close()
			if waitErr := downloader.waitRetry(ctx, attempt); waitErr != nil {
				return waitErr
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			status := response.StatusCode
			_ = response.Body.Close()
			return fmt.Errorf("download server returned HTTP %d", status)
		}
		if response.ContentLength >= 0 && response.ContentLength != options.ExpectedSize {
			_ = response.Body.Close()
			return errors.New("download size changed before transfer")
		}
		reader := &safeDownloadProgressReader{reader: response.Body, progress: progress}
		_, writeErr := writeFileWithinNoFollow(options.Root, options.Relative, reader, secureWriteOptions{
			Mode: options.Mode, ExpectedSize: options.ExpectedSize, ExpectedSHA256: options.ExpectedSHA256,
			Replace: options.Replace, RejectExistingLink: options.RejectLink,
		})
		_ = response.Body.Close()
		if writeErr != nil {
			return writeErr
		}
		progress.complete()
		return nil
	}
	return errors.New("download exhausted retries")
}

type safeDownloadProgressReader struct {
	reader   io.Reader
	progress *safeDownloadProgress
}

func (reader *safeDownloadProgressReader) Read(raw []byte) (int, error) {
	count, err := reader.reader.Read(raw)
	reader.progress.add(int64(count))
	return count, err
}

func validateSafeDownloadRangeResponse(response *http.Response, start, end, total int64) error {
	if response.ContentLength != end-start+1 {
		return errors.New("download byte range returned an unexpected length")
	}
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return errors.New("download byte range used an unexpected content encoding")
	}
	actualStart, actualEnd, actualTotal, err := parseSafeDownloadContentRange(response.Header.Get("Content-Range"))
	if err != nil || actualStart != start || actualEnd != end || actualTotal != total {
		return errors.New("download byte range returned an invalid Content-Range")
	}
	return nil
}

func parseSafeDownloadContentRange(value string) (int64, int64, int64, error) {
	matches := contentRangePattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(matches) != 4 {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	values := make([]int64, 3)
	for index := range values {
		parsed, err := strconv.ParseInt(matches[index+1], 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, 0, errors.New("invalid Content-Range")
		}
		values[index] = parsed
	}
	if values[0] > values[1] || values[1] >= values[2] {
		return 0, 0, 0, errors.New("invalid Content-Range")
	}
	return values[0], values[1], values[2], nil
}

func verifyManagedDownloadSHA(file *managedDownloadFile, size int64, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	if len(expected) != sha256.Size*2 {
		return errors.New("expected download SHA-256 is invalid")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, 0, size)); err != nil {
		return fmt.Errorf("hash segmented download: %w", err)
	}
	if !strings.EqualFold(expected, hex.EncodeToString(hash.Sum(nil))) {
		return errors.New("download SHA-256 does not match the expected value")
	}
	return nil
}

func safeDownloadRequest(ctx context.Context, downloadURL string, headers http.Header) (*http.Request, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, errors.New("download URL provider returned an empty URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, errors.New("create safe download request")
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Accept-Encoding", "identity")
	return request, nil
}

func (downloader *safeSegmentedDownloader) do(request *http.Request, client *http.Client) (*http.Response, error) {
	release, err := downloader.limiter.acquire(request.Context())
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		release()
		return nil, err
	}
	response.Body = &downloadLimitedBody{ReadCloser: response.Body, release: release}
	return response, nil
}

type downloadLimitedBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (body *downloadLimitedBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.release)
	return err
}

func transientDownloadStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (downloader *safeSegmentedDownloader) attempts() int {
	if downloader.maxAttempts <= 0 {
		return safeDownloadMaxAttempts
	}
	return downloader.maxAttempts
}

func (downloader *safeSegmentedDownloader) waitRetry(ctx context.Context, attempt int) error {
	if attempt+1 >= downloader.attempts() {
		return errors.New("download retry limit reached")
	}
	delay := downloader.retryDelay
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}
	delay *= time.Duration(1 << min(attempt, 3))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type safeDownloadURLState struct {
	mu       sync.Mutex
	provider func(context.Context) (string, error)
	current  string
}

func (state *safeDownloadURLState) currentURL(ctx context.Context) (string, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.current != "" {
		return state.current, nil
	}
	value, err := state.provider(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("download URL provider returned an empty URL")
	}
	state.current = value
	return value, nil
}

func (state *safeDownloadURLState) refresh(ctx context.Context, previous string) (string, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.current != "" && state.current != previous {
		return state.current, nil
	}
	value, err := state.provider(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("download URL provider returned an empty URL")
	}
	state.current = value
	return value, nil
}

type safeDownloadProgress struct {
	mu        sync.Mutex
	total     int64
	completed int64
	last      int
	report    func(int)
}

func (progress *safeDownloadProgress) add(count int64) {
	if count <= 0 {
		return
	}
	progress.mu.Lock()
	remaining := max(progress.total-progress.completed, int64(0))
	progress.completed += min(count, remaining)
	percent := 100
	if progress.total > 0 {
		percent = int(progress.completed * 100 / progress.total)
	}
	if percent <= progress.last {
		progress.mu.Unlock()
		return
	}
	progress.last = percent
	if progress.report != nil {
		progress.report(percent)
	}
	progress.mu.Unlock()
}

func (progress *safeDownloadProgress) reset() {
	progress.mu.Lock()
	progress.completed = 0
	progress.mu.Unlock()
}

func (progress *safeDownloadProgress) complete() {
	progress.mu.Lock()
	progress.completed = progress.total
	if progress.last >= 100 {
		progress.mu.Unlock()
		return
	}
	progress.last = 100
	if progress.report != nil {
		progress.report(100)
	}
	progress.mu.Unlock()
}
