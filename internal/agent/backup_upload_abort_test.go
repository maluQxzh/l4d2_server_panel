package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestBackupUploadAbortOnlyRemovesMatchingTrustedStagingFiles(t *testing.T) {
	dataDir := t.TempDir()
	root, err := backupUploadRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	uploadID := uuid.NewString()
	otherID := uuid.NewString()
	for _, name := range []string{uploadID + ".part", uploadID + ".zip", otherID + ".part"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reservation := backupUploadReservation{Version: backupUploadReservationVersion, UploadID: uploadID,
		ExpectedSize: 1, ExpectedSHA256: hashBytes([]byte("x"))}
	raw, err := backupUploadReservationBytes(reservation)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{uploadID + backupUploadReservationSuffix, uploadID + backupUploadReservationTempSuffix} {
		if err := os.WriteFile(filepath.Join(root, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executor := &Executor{config: &Config{DataDir: dataDir}}
	result, err := executor.Execute(context.Background(), domain.Job{Type: "backup.upload.abort", Payload: map[string]any{"upload_id": uploadID}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["upload_id"] != uploadID {
		t.Fatalf("abort result = %#v", result)
	}
	for _, name := range []string{uploadID + ".part", uploadID + ".zip", uploadID + backupUploadReservationSuffix, uploadID + backupUploadReservationTempSuffix} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("matching staging file %q survived: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, otherID+".part")); err != nil {
		t.Fatalf("unrelated staging file was changed: %v", err)
	}
}

func TestBackupUploadAbortFailureDoesNotExposeAgentPath(t *testing.T) {
	dataDir := t.TempDir()
	root, err := backupUploadRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	uploadID := uuid.NewString()
	// A directory at a staging filename is untrusted and cannot be removed by
	// the upload cleanup primitive.
	if err := os.Mkdir(filepath.Join(root, uploadID+".part"), 0o700); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{config: &Config{DataDir: dataDir}}
	_, err = executor.Execute(context.Background(), domain.Job{Type: "backup.upload.abort", Payload: map[string]any{"upload_id": uploadID}}, nil)
	if err == nil {
		t.Fatal("untrusted staging directory was accepted")
	}
	if strings.Contains(err.Error(), dataDir) || strings.Contains(err.Error(), uploadID) {
		t.Fatalf("terminal error exposed Agent staging identity or path: %v", err)
	}
}

func TestBackupUploadAbortRejectsInvalidIDWithoutTouchingStaging(t *testing.T) {
	dataDir := t.TempDir()
	root, err := backupUploadRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	otherID := uuid.NewString()
	path := filepath.Join(root, otherID+".part")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{config: &Config{DataDir: dataDir}}
	if _, err := executor.Execute(context.Background(), domain.Job{Type: "backup.upload.abort", Payload: map[string]any{"upload_id": "../escape"}}, nil); err == nil {
		t.Fatal("invalid upload id was accepted")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "keep" {
		t.Fatalf("unrelated staging changed: %q, %v", raw, err)
	}
}
