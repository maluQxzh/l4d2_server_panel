package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) deleteBackup(job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	if payload.BackupID == "" || !validBackupArchiveName(payload.Name) || payload.ExpectedSize < 0 {
		return nil, errors.New("backup deletion metadata is invalid")
	}
	expectedSHA := strings.ToLower(strings.TrimSpace(payload.ExpectedSHA256))
	if expectedSHA != "" {
		if len(expectedSHA) != 64 {
			return nil, errors.New("backup deletion SHA-256 is invalid")
		}
		if _, err := hex.DecodeString(expectedSHA); err != nil {
			return nil, errors.New("backup deletion SHA-256 is invalid")
		}
	}
	dir := filepath.Join(e.config.DataDir, "backups", safeLogName(job.InstanceID))
	file, err := openManagedFileNoFollow(dir, payload.Name)
	if errors.Is(err, os.ErrNotExist) {
		progress(100, "backup was already absent")
		return map[string]any{"backup_id": payload.BackupID, "name": payload.Name, "already_absent": true}, nil
	}
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		_ = file.Close()
		return nil, errors.New("backup deletion target is not a trusted regular file")
	}
	if payload.ExpectedSize > 0 && info.Size() != payload.ExpectedSize {
		_ = file.Close()
		return nil, errors.New("backup deletion target size changed")
	}
	progress(30, "verifying backup before deletion")
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(file, maxRestoreBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if written != info.Size() || written > maxRestoreBytes {
		return nil, errors.New("backup deletion target changed while reading")
	}
	actualSHA := hex.EncodeToString(hash.Sum(nil))
	if expectedSHA != "" && !strings.EqualFold(actualSHA, expectedSHA) {
		return nil, errors.New("backup deletion target SHA-256 changed")
	}
	if err := deleteManagedFileNoFollow(dir, payload.Name); err != nil {
		return nil, err
	}
	progress(100, "backup deleted")
	return map[string]any{"backup_id": payload.BackupID, "name": payload.Name, "size": info.Size(), "sha256": actualSHA, "already_absent": false}, nil
}

func validBackupArchiveName(name string) bool {
	if name == "" || len(name) > 512 || filepath.Base(name) != name || !strings.HasSuffix(strings.ToLower(name), ".zip") {
		return false
	}
	validPrefix := false
	for _, prefix := range []string{"daily-", "weekly-", "checkpoint-", "manual-"} {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			validPrefix = true
			break
		}
	}
	if !validPrefix {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '.' && char != '-' && char != '_' {
			return false
		}
	}
	return true
}
