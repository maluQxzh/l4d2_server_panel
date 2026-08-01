package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func uploadChunkJob(uploadID string, offset int64, chunk []byte, total []byte) domain.Job {
	return domain.Job{ID: uuid.NewString(), InstanceID: "instance", Type: "backup.upload.chunk", Payload: map[string]any{
		"upload_id": uploadID, "offset": offset, "length": len(chunk), "chunk_sha256": hashBytes(chunk),
		"content_base64": base64.StdEncoding.EncodeToString(chunk), "expected_size": len(total),
		"expected_sha256": hashBytes(total), "expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}}
}

func executeUploadChunkForTest(executor *Executor, job domain.Job) (map[string]any, error) {
	return executor.backupUploadChunkWithLimits(context.Background(), job, func(int, string) {}, defaultBackupUploadLimits, func(string) (uint64, error) {
		return ^uint64(0), nil
	})
}

func TestBackupUploadChunksCommitInOrderAndCompleteAtomically(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	uploadID := uuid.NewString()
	first, second := []byte("portable "), []byte("backup archive")
	total := append(append([]byte(nil), first...), second...)

	result, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 0, first, total))
	if err != nil {
		t.Fatal(err)
	}
	if result["complete"] != false || result["received_size"] != int64(len(first)) {
		t.Fatalf("unexpected first result: %#v", result)
	}
	root := filepath.Join(dataDir, "backup-uploads")
	partPath, finalPath := filepath.Join(root, uploadID+".part"), filepath.Join(root, uploadID+".zip")
	if raw, err := os.ReadFile(partPath); err != nil || string(raw) != string(first) {
		t.Fatalf("staging content = %q, %v", raw, err)
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("completed path appeared before final verification: %v", err)
	}

	finalJob := uploadChunkJob(uploadID, int64(len(first)), second, total)
	result, err = executeUploadChunkForTest(executor, finalJob)
	if err != nil {
		t.Fatal(err)
	}
	if result["complete"] != true || result["file_sha256"] != hashBytes(total) {
		t.Fatalf("unexpected final result: %#v", result)
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("part file survived atomic completion: %v", err)
	}
	if raw, err := os.ReadFile(finalPath); err != nil || string(raw) != string(total) {
		t.Fatalf("completed upload = %q, %v", raw, err)
	}
	stale := time.Now().Add(-backupUploadFinalAge - time.Hour)
	if err := os.Chtimes(finalPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if replay, err := executeUploadChunkForTest(executor, finalJob); err != nil || replay["complete"] != true {
		t.Fatalf("completed final chunk was not idempotent after Agent restart: %#v, %v", replay, err)
	}
	if info, err := os.Stat(finalPath); err != nil || !info.ModTime().After(stale) {
		t.Fatalf("completed validation did not renew the final upload lease: info=%v err=%v", info, err)
	}
	if err := os.Chtimes(finalPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	resolved, err := executor.resolveUploadedBackup(uploadID, hashBytes(total), int64(len(total)))
	if err != nil || !samePath(resolved, finalPath) {
		t.Fatalf("resolve completed upload = %q, %v", resolved, err)
	}
	if info, err := os.Stat(finalPath); err != nil || !info.ModTime().After(stale) {
		t.Fatalf("restore verification did not renew the final upload lease: info=%v err=%v", info, err)
	}
	if err := removeBackupUploadFiles(dataDir, uploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("completed upload was not cleaned: %v", err)
	}
}

func TestRestoreConsumesCompletedBackupUpload(t *testing.T) {
	executor, dataDir, instancePath := uploadedPackageExecutor(t)
	manager, err := NewProcessManager(dataDir, executor.logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor.processes = manager
	manager.MarkRecoveryFailed("instance", "previous automatic recovery failed")
	if manager.HasActiveProcess("instance") {
		t.Fatal("stopped degraded recovery marker was treated as an active process")
	}
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "left4dead2", "cfg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "left4dead2", "cfg", "server.cfg"), []byte("hostname uploaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "portable.zip")
	if _, err := CreateBackup("source-instance", source, archive); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxBackupUploadChunk {
		t.Fatalf("test archive unexpectedly exceeds one chunk: %d", len(raw))
	}
	uploadID := uuid.NewString()
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 0, raw, raw)); err != nil {
		t.Fatal(err)
	}
	result, err := executor.restore(context.Background(), domain.Job{ID: uuid.NewString(), InstanceID: "instance", Type: "restore", Payload: map[string]any{
		"upload_id": uploadID, "expected_size": len(raw), "expected_sha256": hashBytes(raw),
	}}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["upload_cleaned"] != true || result["upload_id"] != uploadID {
		t.Fatalf("restore did not report upload cleanup: %#v", result)
	}
	if restored, err := os.ReadFile(filepath.Join(instancePath, "left4dead2", "cfg", "server.cfg")); err != nil || string(restored) != "hostname uploaded\n" {
		t.Fatalf("uploaded backup was not restored: %q, %v", restored, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "backup-uploads", uploadID+".zip")); !os.IsNotExist(err) {
		t.Fatalf("successful restore retained uploaded archive: %v", err)
	}
}

func TestBackupUploadRejectsOutOfOrderAndRollsBackBadFinalHash(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	uploadID := uuid.NewString()
	total := []byte("abcdef")
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 3, total[3:], total)); err == nil {
		t.Fatal("out-of-order first chunk was accepted")
	}
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 0, total[:3], total)); err != nil {
		t.Fatal(err)
	}
	final := uploadChunkJob(uploadID, 3, total[3:], total)
	final.Payload["expected_sha256"] = hashBytes([]byte("different archive"))
	if _, err := executeUploadChunkForTest(executor, final); err == nil {
		t.Fatal("invalid final archive checksum was accepted")
	}
	partPath := filepath.Join(dataDir, "backup-uploads", uploadID+".part")
	info, err := os.Stat(partPath)
	if err != nil || info.Size() != 3 {
		t.Fatalf("failed final chunk was not rolled back to the committed offset: size=%v err=%v", func() int64 {
			if info == nil {
				return -1
			}
			return info.Size()
		}(), err)
	}
}

func TestBackupUploadQuotaDecisionBoundaries(t *testing.T) {
	currentID := uuid.NewString()
	otherID := uuid.NewString()
	currentReservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: currentID, ExpectedSize: 20, ExpectedSHA256: strings.Repeat("a", 64)}
	otherReservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: otherID, ExpectedSize: 20, ExpectedSHA256: strings.Repeat("b", 64)}
	usage := backupUploadUsage{
		sessions: map[string]struct{}{currentID: {}, otherID: {}}, unknownBytes: 10,
		bySession: map[string]*backupUploadSessionUsage{
			currentID: {reservation: &currentReservation, dataBytes: 10, largestDataBytes: 10, hasPart: true},
			otherID:   {reservation: &otherReservation, dataBytes: 20, largestDataBytes: 20, hasFinal: true},
		},
	}
	limits := backupUploadLimits{maxSessions: 2, maxBytes: 50, minFree: 7}

	if err := validateBackupUploadCapacity(usage, 17, 0, limits); err != nil {
		t.Fatalf("exact byte and disk capacity boundaries were rejected: %v", err)
	}
	thirdID := uuid.NewString()
	thirdReservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: thirdID, ExpectedSize: 1, ExpectedSHA256: strings.Repeat("c", 64)}
	usage.sessions[thirdID] = struct{}{}
	usage.bySession[thirdID] = &backupUploadSessionUsage{reservation: &thirdReservation}
	if err := validateBackupUploadCapacity(usage, 18, 0, backupUploadLimits{maxSessions: 2, maxBytes: 100, minFree: 7}); err == nil || !strings.Contains(err.Error(), "session limit") {
		t.Fatalf("new session at the cap was accepted: %v", err)
	}
	delete(usage.sessions, thirdID)
	delete(usage.bySession, thirdID)
	if err := validateBackupUploadCapacity(usage, 17, 0, backupUploadLimits{maxSessions: 2, maxBytes: 49, minFree: 7}); err == nil || !strings.Contains(err.Error(), "staging limit") {
		t.Fatalf("projected aggregate over the byte cap was accepted: %v", err)
	}
	if err := validateBackupUploadCapacity(usage, 16, 0, limits); err == nil || !strings.Contains(err.Error(), "safety margin") {
		t.Fatalf("completion without the free-space margin was accepted: %v", err)
	}
}

func TestBackupUploadReservationsRejectFourthMaximumArchiveBeforeWriting(t *testing.T) {
	usage := backupUploadUsage{sessions: map[string]struct{}{}, bySession: map[string]*backupUploadSessionUsage{}}
	limits := backupUploadLimits{maxSessions: maxBackupUploadSessions, maxBytes: maxBackupUploadStagingBytes, minFree: backupUploadDiskSafetyMargin}
	free := uint64(100 << 30)
	for index := 0; index < 4; index++ {
		id := uuid.NewString()
		reservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: id,
			ExpectedSize: maxUploadedBackup, ExpectedSHA256: strings.Repeat(fmt.Sprintf("%x", index+1), 64)}
		projected, metadataGrowth, err := projectBackupUploadReservation(usage, reservation)
		if err != nil {
			t.Fatal(err)
		}
		err = validateBackupUploadCapacity(projected, free, metadataGrowth, limits)
		if index < 3 {
			if err != nil {
				t.Fatalf("reservation %d was unexpectedly rejected: %v", index+1, err)
			}
			usage = projected
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "staging limit") {
			t.Fatalf("fourth 20 GiB reservation was accepted: %v", err)
		}
		if _, exists := usage.sessions[id]; exists {
			t.Fatal("rejected reservation mutated committed usage")
		}
	}
}

func TestBackupUploadLegacyPartIsConservativeThenUpgraded(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	uploadID := uuid.NewString()
	total := []byte("abcdef")
	root, err := backupUploadRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(root, uploadID+".part")
	if err := os.WriteFile(partPath, total[:3], 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := inspectBackupUploadUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	reserved, _, err := backupUploadReservedBytes(usage)
	if err != nil || reserved != maxUploadedBackup {
		t.Fatalf("legacy reservation = %d, %v; want %d", reserved, err, maxUploadedBackup)
	}
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 3, total[3:], total)); err != nil {
		t.Fatal(err)
	}
	reservationPath, tempPath, err := backupUploadReservationPaths(root, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := readBackupUploadReservation(root, reservationPath, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ExpectedSize != int64(len(total)) || reservation.ExpectedSHA256 != hashBytes(total) {
		t.Fatalf("legacy reservation was not upgraded: %#v", reservation)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("temporary reservation survived upgrade: %v", err)
	}
	usage, err = inspectBackupUploadUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	reserved, _, err = backupUploadReservedBytes(usage)
	if err != nil || reserved <= int64(len(total)) || reserved >= maxUploadedBackup {
		t.Fatalf("upgraded reservation accounting = %d, %v", reserved, err)
	}
}

func TestBackupUploadContinuationMustMatchDurableReservation(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	uploadID := uuid.NewString()
	total := []byte("abcdef")
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 0, total[:3], total)); err != nil {
		t.Fatal(err)
	}
	conflicting := uploadChunkJob(uploadID, 3, total[3:], total)
	conflicting.Payload["expected_sha256"] = strings.Repeat("f", 64)
	if _, err := executeUploadChunkForTest(executor, conflicting); err == nil || !strings.Contains(err.Error(), "reservation") {
		t.Fatalf("conflicting continuation was accepted: %v", err)
	}
	partPath := filepath.Join(dataDir, "backup-uploads", uploadID+".part")
	if raw, err := os.ReadFile(partPath); err != nil || string(raw) != string(total[:3]) {
		t.Fatalf("conflicting continuation changed committed data: %q, %v", raw, err)
	}
}

func TestBackupUploadReservationRejectsCorruptionAndRecoversAtomicTemp(t *testing.T) {
	root, err := backupUploadRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	corruptID := uuid.NewString()
	corruptPath := filepath.Join(root, corruptID+backupUploadReservationSuffix)
	if err := os.WriteFile(corruptPath, []byte(`{"version":1,"upload_id":"`+corruptID+`","expected_size":1,"expected_sha256":"`+strings.Repeat("a", 64)+`","extra":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectBackupUploadUsage(root); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("corrupt reservation was accepted: %v", err)
	}
	if err := os.Remove(corruptPath); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		insecureID := uuid.NewString()
		insecure := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: insecureID,
			ExpectedSize: 1, ExpectedSHA256: strings.Repeat("c", 64)}
		raw, err := backupUploadReservationBytes(insecure)
		if err != nil {
			t.Fatal(err)
		}
		insecurePath := filepath.Join(root, insecureID+backupUploadReservationSuffix)
		if err := os.WriteFile(insecurePath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectBackupUploadUsage(root); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("insecure reservation permissions were accepted: %v", err)
		}
		if err := os.Remove(insecurePath); err != nil {
			t.Fatal(err)
		}
	}
	uploadID := uuid.NewString()
	reservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: uploadID,
		ExpectedSize: 7, ExpectedSHA256: strings.Repeat("b", 64)}
	reservationPath, tempPath, err := backupUploadReservationPaths(root, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := backupUploadReservationBytes(reservation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureBackupUploadReservation(root, reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("atomic temp was not promoted: %v", err)
	}
	if stored, err := readBackupUploadReservation(root, reservationPath, uploadID); err != nil || stored != reservation {
		t.Fatalf("promoted reservation = %#v, %v", stored, err)
	}
}

func TestInspectBackupUploadUsageCountsSessionsOnceAndAllBytes(t *testing.T) {
	root, err := backupUploadRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstID, secondID := uuid.NewString(), uuid.NewString()
	files := map[string]string{
		firstID + ".part": "abc",
		firstID + ".zip":  "defg",
		secondID + ".zip": "hijkl",
		"operator-note":   "mn",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := inspectBackupUploadUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.sessions) != 2 || usage.actualBytes != 14 {
		t.Fatalf("usage = sessions:%d bytes:%d, want sessions:2 bytes:14", len(usage.sessions), usage.actualBytes)
	}
	reserved, outstanding, err := backupUploadReservedBytes(usage)
	if err != nil || reserved != 2*maxUploadedBackup+2 || outstanding != 0 {
		t.Fatalf("legacy/unknown accounting = reserved:%d outstanding:%d err:%v", reserved, outstanding, err)
	}
}

func TestBackupUploadQuotaFailurePreservesExistingPart(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	uploadID := uuid.NewString()
	total := []byte("abcdef")
	limits := backupUploadLimits{maxSessions: 2, maxBytes: 1024, minFree: 10}
	if _, err := executor.backupUploadChunkWithLimits(context.Background(), uploadChunkJob(uploadID, 0, total[:3], total), func(int, string) {}, limits, func(string) (uint64, error) {
		return 1024, nil
	}); err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(dataDir, "backup-uploads", uploadID+".part")
	before, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.backupUploadChunkWithLimits(context.Background(), uploadChunkJob(uploadID, 3, total[3:], total), func(int, string) {}, limits, func(string) (uint64, error) {
		return 12, nil
	}); err == nil || !strings.Contains(err.Error(), "safety margin") {
		t.Fatalf("low-space chunk was accepted: %v", err)
	}
	after, err := os.ReadFile(partPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("quota rejection changed the existing part: before=%q after=%q err=%v", before, after, err)
	}
}

func TestBackupUploadRefusesSymlinkStagingFile(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	uploadID := uuid.NewString()
	root := filepath.Join(dataDir, "backup-uploads")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do not change"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, uploadID+".part")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	chunk := []byte("payload")
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uploadID, 0, chunk, chunk)); err == nil {
		t.Fatal("symlink staging path was accepted")
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "do not change" {
		t.Fatalf("outside target changed: %q, %v", raw, err)
	}
}

func TestBackupUploadRefusesSymlinkStagingDirectory(t *testing.T) {
	executor, dataDir, _ := uploadedPackageExecutor(t)
	target := filepath.Join(dataDir, "redirected-uploads")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dataDir, "backup-uploads")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	chunk := []byte("payload")
	if _, err := executeUploadChunkForTest(executor, uploadChunkJob(uuid.NewString(), 0, chunk, chunk)); err == nil {
		t.Fatal("symlink staging directory was accepted")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("redirected staging directory was modified: entries=%v err=%v", entries, err)
	}
}

func TestCleanupBackupUploadsOnlyRemovesExpiredCanonicalFiles(t *testing.T) {
	dataDir := t.TempDir()
	uploadID := uuid.NewString()
	expiredFinalID := uuid.NewString()
	freshFinalID := uuid.NewString()
	protectedID := uuid.NewString()
	corruptID := uuid.NewString()
	root := filepath.Join(dataDir, "backup-uploads")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(root, uploadID+".part")
	oldReservationPath := filepath.Join(root, uploadID+backupUploadReservationSuffix)
	expiredFinalPath := filepath.Join(root, expiredFinalID+".zip")
	expiredReservationPath := filepath.Join(root, expiredFinalID+backupUploadReservationSuffix)
	expiredTempPath := filepath.Join(root, expiredFinalID+backupUploadReservationTempSuffix)
	freshFinalPath := filepath.Join(root, freshFinalID+".zip")
	freshReservationPath := filepath.Join(root, freshFinalID+backupUploadReservationSuffix)
	protectedPartPath := filepath.Join(root, protectedID+".part")
	protectedReservationPath := filepath.Join(root, protectedID+backupUploadReservationSuffix)
	corruptReservationPath := filepath.Join(root, corruptID+backupUploadReservationSuffix)
	unknownPath := filepath.Join(root, "keep.txt")
	for _, path := range []string{oldPath, expiredFinalPath, freshFinalPath, protectedPartPath, unknownPath} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for id, paths := range map[string][]string{
		uploadID:       {oldReservationPath},
		expiredFinalID: {expiredReservationPath, expiredTempPath},
		freshFinalID:   {freshReservationPath},
		protectedID:    {protectedReservationPath},
	} {
		reservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: id,
			ExpectedSize: 1, ExpectedSHA256: strings.Repeat("a", 64)}
		raw, err := backupUploadReservationBytes(reservation)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(corruptReservationPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-backupUploadPartAge - time.Hour)
	for _, path := range []string{oldPath, oldReservationPath, protectedPartPath, corruptReservationPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := now.Add(-backupUploadFinalAge)
	for _, path := range []string{expiredFinalPath, expiredReservationPath, expiredTempPath} {
		if err := os.Chtimes(path, cutoff, cutoff); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{freshFinalPath, freshReservationPath} {
		if err := os.Chtimes(path, cutoff.Add(time.Second), cutoff.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := CleanupBackupUploads(dataDir, now)
	if err != nil || removed != 6 {
		t.Fatalf("grouped cleanup removed=%d err=%v", removed, err)
	}
	for _, path := range []string{oldPath, oldReservationPath, expiredFinalPath, expiredReservationPath, expiredTempPath, corruptReservationPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expired grouped entry %s survived: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{freshFinalPath, freshReservationPath, protectedPartPath, protectedReservationPath, unknownPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("fresh/protected entry %s was removed: %v", filepath.Base(path), err)
		}
	}
}
