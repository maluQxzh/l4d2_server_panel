package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	maxTransferChunk     = 1 << 20
	maxLogSearchLines    = 1000
	maxLogSearchBytes    = 384 << 10
	maxLogLineBytes      = 64 << 10
	maxLogSegmentBytes   = 128 << 20
	maxLogSearchSegments = 20
)

type logSearchEntry struct {
	At      *time.Time `json:"at,omitempty"`
	Stream  string     `json:"stream"`
	Message string     `json:"message"`
}

func (e *Executor) searchLogs(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	limit := payload.Limit
	if limit < 1 || limit > maxLogSearchLines {
		return nil, errors.New("log search limit must be between 1 and 1000")
	}
	keyword := strings.TrimSpace(payload.Keyword)
	if len(keyword) > 256 {
		return nil, errors.New("log search keyword exceeds 256 bytes")
	}
	var since, until time.Time
	if payload.Since != "" {
		since, err = time.Parse(time.RFC3339Nano, payload.Since)
		if err != nil {
			return nil, errors.New("invalid log search start time")
		}
	}
	if payload.Until != "" {
		until, err = time.Parse(time.RFC3339Nano, payload.Until)
		if err != nil {
			return nil, errors.New("invalid log search end time")
		}
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return nil, errors.New("log search start time must not be after end time")
	}

	base := filepath.Join(e.processes.logDir, safeLogName(job.InstanceID)+".log")
	paths := make([]string, 0, maxLogSearchSegments)
	for segment := maxLogSearchSegments - 1; segment >= 1; segment-- {
		paths = append(paths, base+"."+strconv.Itoa(segment))
	}
	paths = append(paths, base)
	entries := make([]logSearchEntry, 0, limit)
	matched := 0
	usedBytes := 0
	needle := strings.ToLower(keyword)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || st.Size() > maxLogSegmentBytes {
			return nil, errors.New("log segment is not a regular file or exceeds 128 MiB")
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil, openErr
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), maxLogLineBytes)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return nil, err
			}
			entry := parseStoredLogLine(scanner.Text())
			if entry.At == nil && (!since.IsZero() || !until.IsZero()) {
				continue
			}
			if entry.At != nil && ((!since.IsZero() && entry.At.Before(since)) || (!until.IsZero() && entry.At.After(until))) {
				continue
			}
			if needle != "" && !strings.Contains(strings.ToLower(entry.Message), needle) {
				continue
			}
			matched++
			entryBytes := len(entry.Message) + len(entry.Stream) + 64
			if entryBytes > maxLogSearchBytes {
				entry.Message = entry.Message[:maxLogSearchBytes/2] + "…[truncated]"
				entryBytes = len(entry.Message) + len(entry.Stream) + 64
			}
			for len(entries) >= limit || (usedBytes+entryBytes > maxLogSearchBytes && len(entries) > 0) {
				usedBytes -= len(entries[0].Message) + len(entries[0].Stream) + 64
				entries = entries[1:]
			}
			entries = append(entries, entry)
			usedBytes += entryBytes
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return nil, scanErr
		}
	}
	progress(100, "logs searched")
	return map[string]any{"entries": entries, "returned": len(entries), "matched": matched, "truncated": matched > len(entries)}, nil
}

func parseStoredLogLine(line string) logSearchEntry {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) == 3 {
		if parsed, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
			parsed = parsed.UTC()
			return logSearchEntry{At: &parsed, Stream: parts[1], Message: parts[2]}
		}
	}
	return logSearchEntry{Stream: "legacy", Message: line}
}

func (e *Executor) logChunk(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	if payload.Segment < 0 || payload.Segment >= maxLogSearchSegments {
		return nil, fmt.Errorf("log segment must be between 0 and %d", maxLogSearchSegments-1)
	}
	path := filepath.Join(e.processes.logDir, safeLogName(job.InstanceID)+".log")
	if payload.Segment > 0 {
		path += "." + strconv.Itoa(payload.Segment)
	}
	result, err := readVerifiedChunk(path, payload.Offset, payload.Length, payload.ExpectedSHA256, 0)
	if err != nil {
		return nil, err
	}
	result["segment"] = payload.Segment
	progress(100, "log chunk read")
	return result, nil
}

func (e *Executor) backupChunk(_ context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	if payload.BackupID == "" || payload.Archive == "" || payload.ExpectedSHA256 == "" || payload.ExpectedSize <= 0 {
		return nil, errors.New("backup id, archive, expected size, and SHA-256 are required")
	}
	backupDir := filepath.Join(e.config.DataDir, "backups", safeLogName(job.InstanceID))
	if !filepath.IsAbs(payload.Archive) {
		return nil, errors.New("backup archive path must be absolute")
	}
	lexical := filepath.Clean(payload.Archive)
	if st, err := os.Lstat(lexical); err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("backup archive is not a regular file")
	}
	archive, err := resolveBelow(backupDir, lexical)
	if err != nil {
		return nil, err
	}
	canonicalDir, err := filepath.EvalSymlinks(backupDir)
	if err != nil || !samePath(filepath.Dir(archive), canonicalDir) || !strings.EqualFold(filepath.Ext(archive), ".zip") {
		return nil, errors.New("backup archive is outside the instance backup directory")
	}
	result, err := e.readVerifiedBackupChunk(archive, payload.Offset, payload.Length, payload.ExpectedSHA256, payload.ExpectedSize)
	if err != nil {
		return nil, err
	}
	result["backup_id"] = payload.BackupID
	progress(100, "backup chunk read")
	return result, nil
}

type transferVerification struct {
	info      os.FileInfo
	sha256    string
	expiresAt time.Time
}

func (e *Executor) readVerifiedBackupChunk(path string, offset int64, length int, expectedSHA string, expectedSize int64) (map[string]any, error) {
	if offset < 0 || length < 1 || length > maxTransferChunk {
		return nil, errors.New("chunk offset or length is outside the allowed range")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return nil, errors.New("backup transfer source changed")
	}
	if offset > info.Size() {
		return nil, errors.New("chunk offset is beyond end of file")
	}
	cacheKey := path + "\x00" + strings.ToLower(expectedSHA) + "\x00" + strconv.FormatInt(expectedSize, 10)
	now := time.Now()
	verified := false
	e.transferMu.Lock()
	if e.transferVerifications == nil {
		e.transferVerifications = make(map[string]transferVerification)
	}
	for key, cached := range e.transferVerifications {
		if now.After(cached.expiresAt) {
			delete(e.transferVerifications, key)
		}
	}
	if cached, ok := e.transferVerifications[cacheKey]; ok && os.SameFile(cached.info, info) && cached.info.Size() == info.Size() && cached.info.ModTime().Equal(info.ModTime()) {
		verified = true
	}
	e.transferMu.Unlock()
	if !verified {
		hasher := sha256.New()
		if _, err := io.Copy(hasher, f); err != nil {
			return nil, err
		}
		digest := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(digest, expectedSHA) {
			return nil, errors.New("backup transfer source SHA-256 does not match")
		}
		e.transferMu.Lock()
		if len(e.transferVerifications) >= 128 {
			for key := range e.transferVerifications {
				delete(e.transferVerifications, key)
				break
			}
		}
		e.transferVerifications[cacheKey] = transferVerification{info: info, sha256: digest, expiresAt: now.Add(15 * time.Minute)}
		e.transferMu.Unlock()
	}
	remaining := info.Size() - offset
	if int64(length) > remaining {
		length = int(remaining)
	}
	raw := make([]byte, length)
	n, err := f.ReadAt(raw, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	raw = raw[:n]
	post, err := f.Stat()
	if err != nil || !os.SameFile(info, post) || post.Size() != info.Size() || !post.ModTime().Equal(info.ModTime()) {
		e.transferMu.Lock()
		delete(e.transferVerifications, cacheKey)
		e.transferMu.Unlock()
		return nil, errors.New("backup transfer source changed while it was being read")
	}
	chunkDigest := sha256.Sum256(raw)
	next := offset + int64(n)
	if next >= info.Size() {
		e.transferMu.Lock()
		delete(e.transferVerifications, cacheKey)
		e.transferMu.Unlock()
	}
	return map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(raw), "offset": offset, "length": n,
		"next_offset": next, "total_size": info.Size(), "eof": next >= info.Size(),
		"file_sha256": strings.ToLower(expectedSHA), "chunk_sha256": hex.EncodeToString(chunkDigest[:]),
	}, nil
}

func readVerifiedChunk(path string, offset int64, length int, expectedSHA string, expectedSize int64) (map[string]any, error) {
	if offset < 0 || length < 1 || length > maxTransferChunk {
		return nil, errors.New("chunk offset or length is outside the allowed range")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("transfer source is not a regular file")
	}
	if expectedSize > 0 && st.Size() != expectedSize {
		return nil, errors.New("transfer source size changed")
	}
	if offset > st.Size() {
		return nil, errors.New("chunk offset is beyond end of file")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return nil, err
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA != "" && !strings.EqualFold(digest, expectedSHA) {
		return nil, errors.New("transfer source SHA-256 does not match")
	}
	remaining := st.Size() - offset
	if int64(length) > remaining {
		length = int(remaining)
	}
	raw := make([]byte, length)
	n, err := f.ReadAt(raw, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	raw = raw[:n]
	post, err := f.Stat()
	if err != nil || post.Size() != st.Size() || !post.ModTime().Equal(st.ModTime()) {
		return nil, errors.New("transfer source changed while it was being read")
	}
	chunkDigest := sha256.Sum256(raw)
	next := offset + int64(n)
	return map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(raw), "offset": offset, "length": n,
		"next_offset": next, "total_size": st.Size(), "eof": next >= st.Size(),
		"file_sha256": digest, "chunk_sha256": hex.EncodeToString(chunkDigest[:]),
	}, nil
}
