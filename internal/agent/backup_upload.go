package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/shirou/gopsutil/v4/disk"
)

const (
	maxBackupUploadChunk = 1 << 20
	maxUploadedBackup    = int64(20 << 30)
	// The session cap bounds inode/handle pressure, while the byte cap allows
	// three maximum-size archives without turning the staging area into an
	// unbounded cache. Durable per-session reservations prevent concurrent
	// archives from partially filling the cap and deadlocking one another.
	// Each write also reserves enough real free space to finish every admitted
	// archive while leaving the game node a 5 GiB cushion.
	maxBackupUploadSessions      = 32
	maxBackupUploadStagingBytes  = int64(64 << 30)
	backupUploadDiskSafetyMargin = uint64(5 << 30)
	// Control keeps an idle upload for 24 hours. Partial sessions get one hour
	// of cleanup slack; completed archives retain a 48-hour retry lease that is
	// renewed on validation/restore.
	backupUploadPartAge               = 25 * time.Hour
	backupUploadFinalAge              = 48 * time.Hour
	backupUploadReservationVersion    = 1
	backupUploadReservationSuffix     = ".upload.json"
	backupUploadReservationTempSuffix = ".upload.json.tmp"
	maxBackupUploadReservationBytes   = 4096
)

type backupUploadLimits struct {
	maxSessions int
	maxBytes    int64
	minFree     uint64
}

type backupUploadUsage struct {
	sessions     map[string]struct{}
	actualBytes  int64
	unknownBytes int64
	bySession    map[string]*backupUploadSessionUsage
}

type backupUploadReservation struct {
	Version        int    `json:"version"`
	UploadID       string `json:"upload_id"`
	ExpectedSize   int64  `json:"expected_size"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

type backupUploadSessionUsage struct {
	reservation      *backupUploadReservation
	reservationPath  string
	tempReservation  *backupUploadReservation
	tempPath         string
	metadataBytes    int64
	dataBytes        int64
	largestDataBytes int64
	hasPart          bool
	hasFinal         bool
}

var backupUploadMu sync.Mutex

var defaultBackupUploadLimits = backupUploadLimits{
	maxSessions: maxBackupUploadSessions,
	maxBytes:    maxBackupUploadStagingBytes,
	minFree:     backupUploadDiskSafetyMargin,
}

func (e *Executor) backupUploadChunk(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	return e.backupUploadChunkWithLimits(ctx, job, progress, defaultBackupUploadLimits, backupUploadFreeBytes)
}

func (e *Executor) backupUploadChunkWithLimits(ctx context.Context, job domain.Job, progress ProgressFunc, limits backupUploadLimits, freeBytes func(string) (uint64, error)) (map[string]any, error) {
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	uploadID, err := canonicalUploadID(payload.UploadID)
	if err != nil {
		return nil, err
	}
	if payload.Offset < 0 || payload.Length < 1 || payload.Length > maxBackupUploadChunk ||
		payload.ExpectedSize < 1 || payload.ExpectedSize > maxUploadedBackup ||
		payload.Offset+int64(payload.Length) > payload.ExpectedSize ||
		!validUploadSHA256(payload.ChunkSHA256) || !validUploadSHA256(payload.ExpectedSHA256) {
		return nil, errors.New("backup upload chunk metadata is invalid")
	}
	raw, err := base64.StdEncoding.DecodeString(payload.ContentBase64)
	if err != nil || len(raw) != payload.Length {
		return nil, errors.New("backup upload chunk body length is invalid")
	}
	chunkDigest := sha256.Sum256(raw)
	chunkSHA := hex.EncodeToString(chunkDigest[:])
	if !strings.EqualFold(chunkSHA, payload.ChunkSHA256) {
		return nil, errors.New("backup upload chunk SHA-256 mismatch")
	}
	root, partPath, finalPath, err := backupUploadPaths(e.config.DataDir, uploadID)
	if err != nil {
		return nil, err
	}
	backupUploadMu.Lock()
	defer backupUploadMu.Unlock()
	currentSize, err := backupUploadPartSize(root, partPath)
	if err != nil {
		return nil, err
	}
	usage, err := inspectBackupUploadUsage(root)
	if err != nil {
		return nil, err
	}
	reservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: uploadID,
		ExpectedSize: payload.ExpectedSize, ExpectedSHA256: strings.ToLower(payload.ExpectedSHA256)}
	if err := validateReservationMatch(usage.bySession[uploadID], reservation); err != nil {
		return nil, err
	}
	finalExists := false
	if _, err := os.Lstat(finalPath); err == nil {
		finalExists = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if !finalExists && (currentSize < payload.Offset || currentSize > payload.Offset+int64(payload.Length)) {
		return nil, fmt.Errorf("backup upload staging size %d does not match expected offset %d", currentSize, payload.Offset)
	}
	var replayResult map[string]any
	if finalExists {
		var replayOK bool
		replayResult, replayOK, err = completedUploadReplay(root, finalPath, payload, raw, chunkSHA)
		if err != nil {
			return nil, err
		}
		if !replayOK {
			return nil, errors.New("completed backup upload does not match this chunk")
		}
	}
	available, err := freeBytes(root)
	if err != nil {
		return nil, fmt.Errorf("measure backup upload staging free space: %w", err)
	}
	projected, metadataGrowth, err := projectBackupUploadReservation(usage, reservation)
	if err != nil {
		return nil, err
	}
	if err := validateBackupUploadCapacity(projected, available, metadataGrowth, limits); err != nil {
		return nil, err
	}
	if err := ensureBackupUploadReservation(root, reservation); err != nil {
		return nil, err
	}
	if finalExists {
		if err := renewBackupUploadSessionLease(root, uploadID, time.Now()); err != nil {
			return nil, fmt.Errorf("renew completed backup upload lease: %w", err)
		}
		progress(100, "backup upload chunk already committed")
		return replayResult, nil
	}
	f, err := openBackupUploadPart(root, partPath)
	if err != nil {
		return nil, err
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != currentSize {
		return nil, errors.New("backup upload staging file changed during quota validation")
	}
	// A prior failed/crashed attempt may have written only part of this exact
	// chunk. The control plane has not advanced the offset, so roll back only
	// those uncommitted bytes before retrying.
	if info.Size() > payload.Offset {
		if err := f.Truncate(payload.Offset); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	written, writeErr := f.WriteAt(raw, payload.Offset)
	if writeErr != nil || written != len(raw) {
		_ = f.Truncate(payload.Offset)
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		_ = f.Truncate(payload.Offset)
		return nil, err
	}
	next := payload.Offset + int64(len(raw))
	post, err := f.Stat()
	if err != nil || post.Size() != next || !post.Mode().IsRegular() || isReparseOrSymlink(post) {
		_ = f.Truncate(payload.Offset)
		return nil, errors.New("backup upload staging file changed while writing")
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	closeFile = false
	result := map[string]any{"upload_id": uploadID, "offset": payload.Offset, "length": len(raw),
		"received_size": next, "chunk_sha256": chunkSHA, "complete": next == payload.ExpectedSize}
	if next < payload.ExpectedSize {
		if err := renewBackupUploadSessionLease(root, uploadID, time.Now()); err != nil {
			return nil, fmt.Errorf("renew partial backup upload lease: %w", err)
		}
		progress(100, "backup upload chunk committed")
		return result, nil
	}
	progress(85, "verifying completed backup upload")
	fileSHA, size, err := hashFile(partPath)
	if err != nil {
		return nil, err
	}
	if size != payload.ExpectedSize || !strings.EqualFold(fileSHA, payload.ExpectedSHA256) {
		if truncateErr := truncateSafeUploadPart(root, partPath, payload.Offset); truncateErr != nil {
			return nil, fmt.Errorf("completed backup checksum mismatch; rollback chunk: %w", truncateErr)
		}
		return nil, errors.New("completed backup upload SHA-256 mismatch")
	}
	if err := os.Rename(partPath, finalPath); err != nil {
		return nil, fmt.Errorf("atomically complete backup upload: %w", err)
	}
	if err := syncBackupUploadDirectory(root); err != nil {
		return nil, fmt.Errorf("persist completed backup upload rename: %w", err)
	}
	if err := validateUploadRegularFile(root, finalPath); err != nil {
		return nil, err
	}
	if err := renewBackupUploadSessionLease(root, uploadID, time.Now()); err != nil {
		return nil, fmt.Errorf("renew completed backup upload lease: %w", err)
	}
	result["file_sha256"] = strings.ToLower(fileSHA)
	progress(100, "backup upload completed")
	return result, nil
}

func canonicalUploadID(value string) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || parsed.String() != strings.ToLower(strings.TrimSpace(value)) {
		return "", errors.New("backup upload id is invalid")
	}
	return parsed.String(), nil
}

func validUploadSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func backupUploadFreeBytes(root string) (uint64, error) {
	usage, err := disk.Usage(root)
	if err != nil {
		return 0, err
	}
	return usage.Free, nil
}

func backupUploadPartSize(root, partPath string) (int64, error) {
	info, err := os.Lstat(partPath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || isReparseOrSymlink(info) || validateUploadRegularFile(root, partPath) != nil {
		return 0, errors.New("backup upload staging file is not a trusted regular file")
	}
	return info.Size(), nil
}

func inspectBackupUploadUsage(root string) (backupUploadUsage, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return backupUploadUsage{}, err
	}
	usage := backupUploadUsage{sessions: make(map[string]struct{}), bySession: make(map[string]*backupUploadSessionUsage)}
	for _, entry := range entries {
		path, resolveErr := ResolveWithin(root, entry.Name())
		info, infoErr := entry.Info()
		if resolveErr != nil || infoErr != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) || validateUploadRegularFile(root, path) != nil {
			return backupUploadUsage{}, fmt.Errorf("backup upload staging contains an untrusted entry %q", entry.Name())
		}
		if info.Size() < 0 || addInt64Overflow(usage.actualBytes, info.Size()) {
			return backupUploadUsage{}, errors.New("backup upload staging byte accounting overflow")
		}
		usage.actualBytes += info.Size()
		id, kind := classifyBackupUploadEntry(entry.Name())
		if id == "" {
			if addInt64Overflow(usage.unknownBytes, info.Size()) {
				return backupUploadUsage{}, errors.New("backup upload unknown byte accounting overflow")
			}
			usage.unknownBytes += info.Size()
			continue
		}
		usage.sessions[id] = struct{}{}
		session := usage.bySession[id]
		if session == nil {
			session = &backupUploadSessionUsage{}
			usage.bySession[id] = session
		}
		switch kind {
		case "part", "final":
			if addInt64Overflow(session.dataBytes, info.Size()) {
				return backupUploadUsage{}, errors.New("backup upload session byte accounting overflow")
			}
			session.dataBytes += info.Size()
			if info.Size() > session.largestDataBytes {
				session.largestDataBytes = info.Size()
			}
			if kind == "part" {
				session.hasPart = true
			} else {
				session.hasFinal = true
			}
		case "reservation", "reservation-temp":
			reservation, readErr := readBackupUploadReservation(root, path, id)
			if readErr != nil {
				return backupUploadUsage{}, fmt.Errorf("read backup upload reservation %q: %w", entry.Name(), readErr)
			}
			if addInt64Overflow(session.metadataBytes, info.Size()) {
				return backupUploadUsage{}, errors.New("backup upload metadata byte accounting overflow")
			}
			session.metadataBytes += info.Size()
			if kind == "reservation" {
				session.reservation, session.reservationPath = &reservation, path
			} else {
				session.tempReservation, session.tempPath = &reservation, path
			}
		}
	}
	for id, session := range usage.bySession {
		if session.reservation != nil && session.tempReservation != nil && *session.reservation != *session.tempReservation {
			return backupUploadUsage{}, fmt.Errorf("backup upload %s has conflicting reservation metadata", id)
		}
	}
	return usage, nil
}

func validateBackupUploadCapacity(usage backupUploadUsage, freeBytes uint64, metadataGrowth int64, limits backupUploadLimits) error {
	if limits.maxSessions < 1 || limits.maxBytes < 1 || metadataGrowth < 0 {
		return errors.New("backup upload capacity inputs are invalid")
	}
	if len(usage.sessions) > limits.maxSessions {
		return fmt.Errorf("backup upload staging session limit reached (%d)", limits.maxSessions)
	}
	reserved, outstanding, err := backupUploadReservedBytes(usage)
	if err != nil {
		return err
	}
	if reserved > limits.maxBytes {
		return fmt.Errorf("reserved backup uploads would exceed the %d-byte staging limit", limits.maxBytes)
	}
	if metadataGrowth > 0 {
		if addInt64Overflow(outstanding, metadataGrowth) {
			return errors.New("backup upload disk reservation accounting overflow")
		}
		outstanding += metadataGrowth
	}
	if freeBytes < limits.minFree || uint64(outstanding) > freeBytes-limits.minFree {
		return fmt.Errorf("insufficient disk space for all reserved backup uploads while retaining %d bytes of safety margin", limits.minFree)
	}
	return nil
}

func classifyBackupUploadEntry(name string) (string, string) {
	for _, candidate := range []struct{ suffix, kind string }{
		{backupUploadReservationTempSuffix, "reservation-temp"},
		{backupUploadReservationSuffix, "reservation"},
		{".part", "part"},
		{".zip", "final"},
	} {
		if strings.HasSuffix(name, candidate.suffix) {
			return canonicalUUID(strings.TrimSuffix(name, candidate.suffix)), candidate.kind
		}
	}
	return "", ""
}

func addInt64Overflow(left, right int64) bool {
	return left < 0 || right < 0 || left > int64(^uint64(0)>>1)-right
}

func readBackupUploadReservation(root, path, uploadID string) (backupUploadReservation, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return backupUploadReservation{}, err
	}
	if err := validateUploadRegularFile(root, path); err != nil {
		return backupUploadReservation{}, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return backupUploadReservation{}, errors.New("backup upload reservation permissions must be 0600")
	}
	if info.Size() < 2 || info.Size() > maxBackupUploadReservationBytes {
		return backupUploadReservation{}, errors.New("backup upload reservation size is invalid")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return backupUploadReservation{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var reservation backupUploadReservation
	if err := decoder.Decode(&reservation); err != nil {
		return backupUploadReservation{}, errors.New("backup upload reservation JSON is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return backupUploadReservation{}, errors.New("backup upload reservation contains trailing JSON")
	}
	if reservation.Version != backupUploadReservationVersion || reservation.UploadID != uploadID ||
		reservation.ExpectedSize < 1 || reservation.ExpectedSize > maxUploadedBackup ||
		!validUploadSHA256(reservation.ExpectedSHA256) || reservation.ExpectedSHA256 != strings.ToLower(reservation.ExpectedSHA256) {
		return backupUploadReservation{}, errors.New("backup upload reservation fields are invalid")
	}
	canonical, err := backupUploadReservationBytes(reservation)
	if err != nil || !bytes.Equal(raw, canonical) {
		return backupUploadReservation{}, errors.New("backup upload reservation JSON is not canonical")
	}
	return reservation, nil
}

func backupUploadReservationBytes(reservation backupUploadReservation) ([]byte, error) {
	raw, err := json.Marshal(reservation)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func validateReservationMatch(session *backupUploadSessionUsage, expected backupUploadReservation) error {
	if session == nil {
		return nil
	}
	for _, stored := range []*backupUploadReservation{session.reservation, session.tempReservation} {
		if stored != nil && *stored != expected {
			return errors.New("backup upload reservation does not match the expected size or SHA-256")
		}
	}
	if session.largestDataBytes > expected.ExpectedSize {
		return errors.New("backup upload staging file exceeds its reserved size")
	}
	return nil
}

func projectBackupUploadReservation(usage backupUploadUsage, reservation backupUploadReservation) (backupUploadUsage, int64, error) {
	if err := validateReservationMatch(usage.bySession[reservation.UploadID], reservation); err != nil {
		return backupUploadUsage{}, 0, err
	}
	projected := usage
	projected.sessions = make(map[string]struct{}, len(usage.sessions)+1)
	for id := range usage.sessions {
		projected.sessions[id] = struct{}{}
	}
	projected.bySession = make(map[string]*backupUploadSessionUsage, len(usage.bySession)+1)
	for id, value := range usage.bySession {
		copyValue := *value
		projected.bySession[id] = &copyValue
	}
	projected.sessions[reservation.UploadID] = struct{}{}
	session := projected.bySession[reservation.UploadID]
	if session == nil {
		session = &backupUploadSessionUsage{}
		projected.bySession[reservation.UploadID] = session
	}
	metadataGrowth := int64(0)
	if session.reservation == nil && session.tempReservation == nil {
		raw, err := backupUploadReservationBytes(reservation)
		if err != nil {
			return backupUploadUsage{}, 0, err
		}
		metadataGrowth = int64(len(raw))
		session.metadataBytes += metadataGrowth
		if addInt64Overflow(projected.actualBytes, metadataGrowth) {
			return backupUploadUsage{}, 0, errors.New("backup upload metadata byte accounting overflow")
		}
		projected.actualBytes += metadataGrowth
	}
	copyReservation := reservation
	session.reservation = &copyReservation
	return projected, metadataGrowth, nil
}

func backupUploadReservedBytes(usage backupUploadUsage) (int64, int64, error) {
	reserved, outstanding := usage.unknownBytes, int64(0)
	for _, session := range usage.bySession {
		expected := maxUploadedBackup
		if session.reservation != nil {
			expected = session.reservation.ExpectedSize
		} else if session.tempReservation != nil {
			expected = session.tempReservation.ExpectedSize
		}
		dataCharge := expected
		if session.dataBytes > dataCharge {
			dataCharge = session.dataBytes
		}
		if addInt64Overflow(dataCharge, session.metadataBytes) || addInt64Overflow(reserved, dataCharge+session.metadataBytes) {
			return 0, 0, errors.New("backup upload reservation accounting overflow")
		}
		reserved += dataCharge + session.metadataBytes
		if !session.hasFinal && session.largestDataBytes < expected {
			remaining := expected - session.largestDataBytes
			if addInt64Overflow(outstanding, remaining) {
				return 0, 0, errors.New("backup upload outstanding byte accounting overflow")
			}
			outstanding += remaining
		}
	}
	if reserved < usage.actualBytes {
		return 0, 0, errors.New("backup upload staging contains unaccounted bytes")
	}
	return reserved, outstanding, nil
}

func backupUploadPaths(dataDir, uploadID string) (string, string, string, error) {
	uploadID, err := canonicalUploadID(uploadID)
	if err != nil {
		return "", "", "", err
	}
	root, err := backupUploadRoot(dataDir)
	if err != nil {
		return "", "", "", err
	}
	partPath := filepath.Join(root, uploadID+".part")
	resolvedPart, err := resolveBelow(root, partPath)
	if err != nil {
		return "", "", "", err
	}
	if !samePath(resolvedPart, partPath) {
		return "", "", "", ErrPathEscape
	}
	finalPath := filepath.Join(root, uploadID+".zip")
	resolvedFinal, err := resolveBelow(root, finalPath)
	if err != nil {
		return "", "", "", err
	}
	if !samePath(resolvedFinal, finalPath) {
		return "", "", "", ErrPathEscape
	}
	return root, partPath, finalPath, nil
}

func backupUploadReservationPaths(root, uploadID string) (string, string, error) {
	if canonicalUUID(uploadID) == "" {
		return "", "", errors.New("backup upload id is invalid")
	}
	reservation := filepath.Join(root, uploadID+backupUploadReservationSuffix)
	temp := filepath.Join(root, uploadID+backupUploadReservationTempSuffix)
	for _, path := range []string{reservation, temp} {
		resolved, err := resolveBelow(root, path)
		if err != nil || !samePath(resolved, path) {
			return "", "", ErrPathEscape
		}
	}
	return reservation, temp, nil
}

func ensureBackupUploadReservation(root string, reservation backupUploadReservation) error {
	reservationPath, tempPath, err := backupUploadReservationPaths(root, reservation.UploadID)
	if err != nil {
		return err
	}
	if stored, readErr := readBackupUploadReservation(root, reservationPath, reservation.UploadID); readErr == nil {
		if stored != reservation {
			return errors.New("backup upload reservation does not match the expected size or SHA-256")
		}
		if tempStored, tempErr := readBackupUploadReservation(root, tempPath, reservation.UploadID); tempErr == nil {
			if tempStored != reservation {
				return errors.New("backup upload temporary reservation conflicts with committed metadata")
			}
			if err := os.Remove(tempPath); err != nil {
				return fmt.Errorf("remove redundant backup upload reservation temp file: %w", err)
			}
			if err := syncBackupUploadDirectory(root); err != nil {
				return err
			}
		} else if !os.IsNotExist(tempErr) {
			return tempErr
		}
		return nil
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if stored, readErr := readBackupUploadReservation(root, tempPath, reservation.UploadID); readErr == nil {
		if stored != reservation {
			return errors.New("backup upload temporary reservation does not match the expected size or SHA-256")
		}
		if err := os.Rename(tempPath, reservationPath); err != nil {
			return fmt.Errorf("commit backup upload reservation: %w", err)
		}
		if err := syncBackupUploadDirectory(root); err != nil {
			return err
		}
		return validateCommittedBackupUploadReservation(root, reservationPath, reservation)
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	raw, err := backupUploadReservationBytes(reservation)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create backup upload reservation: %w", err)
	}
	removeTemp := true
	defer func() {
		_ = f.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if written, writeErr := f.Write(raw); writeErr != nil || written != len(raw) {
		if writeErr != nil {
			return writeErr
		}
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := readBackupUploadReservation(root, tempPath, reservation.UploadID); err != nil {
		return err
	}
	if err := os.Rename(tempPath, reservationPath); err != nil {
		return fmt.Errorf("atomically commit backup upload reservation: %w", err)
	}
	removeTemp = false
	if err := syncBackupUploadDirectory(root); err != nil {
		return err
	}
	return validateCommittedBackupUploadReservation(root, reservationPath, reservation)
}

func validateCommittedBackupUploadReservation(root, path string, expected backupUploadReservation) error {
	stored, err := readBackupUploadReservation(root, path, expected.UploadID)
	if err != nil {
		return err
	}
	if stored != expected {
		return errors.New("committed backup upload reservation changed unexpectedly")
	}
	return nil
}

func syncBackupUploadDirectory(root string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func backupUploadRoot(dataDir string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	canonicalData, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", err
	}
	root := filepath.Join(canonicalData, "backup-uploads")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || isReparseOrSymlink(rootInfo) {
		return "", errors.New("backup upload staging directory is not a trusted directory")
	}
	resolved, err := resolveBelow(canonicalData, root)
	if err != nil || !samePath(resolved, root) {
		return "", errors.New("backup upload staging directory escapes the Agent data directory")
	}
	return root, nil
}

func openBackupUploadPart(root, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) {
		_ = f.Close()
		return nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || isReparseOrSymlink(pathInfo) {
		return fail(errors.New("backup upload staging file is not a trusted regular file"))
	}
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(pathInfo, openedInfo) {
		return fail(errors.New("backup upload staging file changed while opening"))
	}
	resolved, err := resolveBelow(root, path)
	if err != nil || !samePath(resolved, path) {
		return fail(ErrPathEscape)
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	return f, nil
}

func validateUploadRegularFile(root, path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) {
		return errors.New("backup upload is not a trusted regular file")
	}
	resolved, err := resolveBelow(root, path)
	if err != nil || !samePath(resolved, path) || !samePath(filepath.Dir(path), root) {
		return ErrPathEscape
	}
	return nil
}

func renewBackupUploadLease(root, path string, now time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || isReparseOrSymlink(pathInfo) {
		return errors.New("backup upload lease target is not a trusted regular file")
	}
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(pathInfo, openedInfo) || validateUploadRegularFile(root, path) != nil {
		return errors.New("backup upload lease target changed while opening")
	}
	if err := os.Chtimes(path, now, now); err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || isReparseOrSymlink(after) || !os.SameFile(openedInfo, after) {
		return errors.New("backup upload lease target changed while renewing")
	}
	return nil
}

func renewBackupUploadSessionLease(root, uploadID string, now time.Time) error {
	reservationPath, tempPath, err := backupUploadReservationPaths(root, uploadID)
	if err != nil {
		return err
	}
	paths := []string{
		filepath.Join(root, uploadID+".part"),
		filepath.Join(root, uploadID+".zip"),
		reservationPath,
		tempPath,
	}
	renewed := 0
	for _, path := range paths {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := renewBackupUploadLease(root, path, now); err != nil {
			return err
		}
		renewed++
	}
	if renewed == 0 {
		return errors.New("backup upload session has no files to renew")
	}
	return nil
}

func truncateSafeUploadPart(root, path string, size int64) error {
	if err := validateUploadRegularFile(root, path); err != nil {
		return err
	}
	return os.Truncate(path, size)
}

func completedUploadReplay(root, finalPath string, payload operationPayload, raw []byte, chunkSHA string) (map[string]any, bool, error) {
	if payload.Offset+int64(payload.Length) != payload.ExpectedSize {
		return nil, false, nil
	}
	info, err := os.Lstat(finalPath)
	if err != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) || info.Size() != payload.ExpectedSize {
		return nil, false, nil
	}
	if err := validateUploadRegularFile(root, finalPath); err != nil {
		return nil, false, err
	}
	f, err := os.Open(finalPath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	chunk := make([]byte, payload.Length)
	if _, err := f.ReadAt(chunk, payload.Offset); err != nil || !strings.EqualFold(chunkSHA, hashBytes(chunk)) || chunkSHA != hashBytes(raw) {
		return nil, false, nil
	}
	fileSHA, size, err := hashFile(finalPath)
	if err != nil || size != payload.ExpectedSize || !strings.EqualFold(fileSHA, payload.ExpectedSHA256) {
		return nil, false, nil
	}
	return map[string]any{"upload_id": payload.UploadID, "offset": payload.Offset, "length": payload.Length,
		"received_size": payload.ExpectedSize, "chunk_sha256": strings.ToLower(chunkSHA), "complete": true,
		"file_sha256": strings.ToLower(fileSHA)}, true, nil
}

func hashBytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (e *Executor) resolveUploadedBackup(uploadID, expectedSHA string, expectedSize int64) (string, error) {
	root, _, finalPath, err := backupUploadPaths(e.config.DataDir, uploadID)
	if err != nil {
		return "", err
	}
	if expectedSize < 1 || expectedSize > maxUploadedBackup || !validUploadSHA256(expectedSHA) {
		return "", errors.New("uploaded backup verification metadata is invalid")
	}
	backupUploadMu.Lock()
	defer backupUploadMu.Unlock()
	if err := validateUploadRegularFile(root, finalPath); err != nil {
		return "", err
	}
	// Renew before hashing so an hourly retention pass cannot treat a long
	// verification or restore attempt as abandoned. A failed restore remains
	// available for the control plane's 24-hour retry window.
	if err := renewBackupUploadSessionLease(root, uploadID, time.Now()); err != nil {
		return "", fmt.Errorf("renew uploaded backup lease: %w", err)
	}
	actualSHA, actualSize, err := hashFile(finalPath)
	if err != nil {
		return "", err
	}
	if actualSize != expectedSize || !strings.EqualFold(actualSHA, expectedSHA) {
		return "", errors.New("uploaded backup size or SHA-256 does not match the restore request")
	}
	usage, err := inspectBackupUploadUsage(root)
	if err != nil {
		return "", err
	}
	reservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: uploadID,
		ExpectedSize: expectedSize, ExpectedSHA256: strings.ToLower(expectedSHA)}
	projected, metadataGrowth, err := projectBackupUploadReservation(usage, reservation)
	if err != nil {
		return "", err
	}
	available, err := backupUploadFreeBytes(root)
	if err != nil {
		return "", fmt.Errorf("measure backup upload staging free space: %w", err)
	}
	if err := validateBackupUploadCapacity(projected, available, metadataGrowth, defaultBackupUploadLimits); err != nil {
		return "", err
	}
	if err := ensureBackupUploadReservation(root, reservation); err != nil {
		return "", err
	}
	if err := renewBackupUploadSessionLease(root, uploadID, time.Now()); err != nil {
		return "", fmt.Errorf("renew verified backup upload lease: %w", err)
	}
	return finalPath, nil
}

func removeBackupUploadFiles(dataDir, uploadID string) error {
	backupUploadMu.Lock()
	defer backupUploadMu.Unlock()
	root, partPath, finalPath, err := backupUploadPaths(dataDir, uploadID)
	if err != nil {
		return err
	}
	reservationPath, tempPath, err := backupUploadReservationPaths(root, uploadID)
	if err != nil {
		return err
	}
	var joined error
	removed := false
	for _, path := range []string{partPath, finalPath, reservationPath, tempPath} {
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) || validateUploadRegularFile(root, path) != nil {
			joined = errors.Join(joined, errors.New("refusing to remove untrusted backup upload path"))
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil {
			joined = errors.Join(joined, removeErr)
		} else {
			removed = true
		}
	}
	if removed {
		joined = errors.Join(joined, syncBackupUploadDirectory(root))
	}
	return joined
}

func CleanupBackupUploads(dataDir string, now time.Time) (int, error) {
	backupUploadMu.Lock()
	defer backupUploadMu.Unlock()
	root, err := backupUploadRoot(dataDir)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	type cleanupEntry struct {
		path string
		kind string
		info os.FileInfo
	}
	groups := make(map[string][]cleanupEntry)
	unsafeGroups := make(map[string]bool)
	var joined error
	for _, entry := range entries {
		id, kind := classifyBackupUploadEntry(entry.Name())
		if id == "" {
			continue
		}
		path, resolveErr := ResolveWithin(root, entry.Name())
		info, infoErr := entry.Info()
		if resolveErr != nil || infoErr != nil || !info.Mode().IsRegular() || isReparseOrSymlink(info) || validateUploadRegularFile(root, path) != nil {
			unsafeGroups[id] = true
			joined = errors.Join(joined, fmt.Errorf("refusing to clean untrusted backup upload entry %q", entry.Name()))
			continue
		}
		groups[id] = append(groups[id], cleanupEntry{path: path, kind: kind, info: info})
	}
	removed := 0
	for id, group := range groups {
		if unsafeGroups[id] {
			continue
		}
		maxAge := backupUploadPartAge
		newest := time.Time{}
		for _, entry := range group {
			if entry.kind == "final" {
				maxAge = backupUploadFinalAge
			}
			if entry.info.ModTime().After(newest) {
				newest = entry.info.ModTime()
			}
		}
		if newest.After(now.Add(-maxAge)) {
			continue
		}
		for _, entry := range group {
			if removeErr := os.Remove(entry.path); removeErr != nil {
				joined = errors.Join(joined, removeErr)
			} else {
				removed++
			}
		}
	}
	if removed > 0 {
		joined = errors.Join(joined, syncBackupUploadDirectory(root))
	}
	return removed, joined
}

func canonicalUUID(value string) string {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != strings.ToLower(value) {
		return ""
	}
	return parsed.String()
}
