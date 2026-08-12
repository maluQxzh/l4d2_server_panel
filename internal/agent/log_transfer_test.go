package agent

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

var transferTestProgress = func(int, string) {}

func newTransferTestExecutor(t *testing.T, instanceID string) (*Executor, string) {
	t.Helper()
	data := t.TempDir()
	registry, err := OpenRegistry(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: instanceID, Name: instanceID, RelativePath: instanceID}); err != nil {
		t.Fatal(err)
	}
	processes, err := NewProcessManager(data, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{config: &Config{DataDir: data}, registry: registry, processes: processes}, data
}

func TestSearchLogsFiltersTimeKeywordAndBoundsResult(t *testing.T) {
	executor, _ := newTransferTestExecutor(t, "instance-1")
	old := time.Now().UTC().Add(-2 * time.Hour)
	recent := time.Now().UTC().Add(-time.Minute)
	path := filepath.Join(executor.processes.logDir, "instance-1.log")
	content := old.Format(time.RFC3339Nano) + "\tstdout\told survivor joined\n" +
		recent.Format(time.RFC3339Nano) + "\tstderr\trecent SURVIVOR joined\n" +
		recent.Format(time.RFC3339Nano) + "\tstdout\tunrelated\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := executor.searchLogs(context.Background(), domain.Job{InstanceID: "instance-1", Payload: map[string]any{
		"keyword": "survivor", "since": recent.Add(-time.Minute).Format(time.RFC3339Nano), "limit": 10,
	}}, transferTestProgress)
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := result["entries"].([]logSearchEntry)
	if !ok || len(entries) != 1 || entries[0].Stream != "stderr" || !strings.Contains(entries[0].Message, "SURVIVOR") {
		t.Fatalf("unexpected search result: %#v", result)
	}
	if result["matched"] != 1 || result["truncated"] != false {
		t.Fatalf("unexpected result metadata: %#v", result)
	}
}

func TestSearchLogsRejectsOversizedSegment(t *testing.T) {
	executor, _ := newTransferTestExecutor(t, "instance-2")
	path := filepath.Join(executor.processes.logDir, "instance-2.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxLogSegmentBytes+1); err != nil {
		t.Fatal(err)
	}
	_, err := executor.searchLogs(context.Background(), domain.Job{InstanceID: "instance-2", Payload: map[string]any{"limit": 10}}, transferTestProgress)
	if err == nil || !strings.Contains(err.Error(), "128 MiB") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}

func TestBackupChunkRequiresInstanceBoundaryAndChecksum(t *testing.T) {
	executor, data := newTransferTestExecutor(t, "instance-3")
	dir := filepath.Join(data, "backups", "instance-3")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "manual.zip")
	content := []byte("portable backup bytes")
	if err := os.WriteFile(archive, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, _, err := hashFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{InstanceID: "instance-3", Payload: map[string]any{"backup_id": "backup-1", "archive": archive,
		"expected_size": len(content), "expected_sha256": digest, "offset": 2, "length": 5}}
	result, err := executor.backupChunk(context.Background(), job, transferTestProgress)
	if err != nil {
		t.Fatal(err)
	}
	if result["length"] != 5 || result["file_sha256"] != digest || result["backup_id"] != "backup-1" {
		t.Fatalf("unexpected chunk result: %#v", result)
	}
	pluginArchive := filepath.Join(dir, "plugin-manual.7z")
	if err := os.WriteFile(pluginArchive, content, 0o600); err != nil {
		t.Fatal(err)
	}
	job.Payload["archive"] = pluginArchive
	if _, err := executor.backupChunk(context.Background(), job, transferTestProgress); err != nil {
		t.Fatalf("plugin 7z download was rejected: %v", err)
	}

	outside := filepath.Join(data, "backups", "outside.zip")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	job.Payload["archive"] = outside
	if _, err := executor.backupChunk(context.Background(), job, transferTestProgress); err == nil {
		t.Fatal("backup path outside the instance directory was accepted")
	}
	job.Payload["archive"] = archive
	job.Payload["expected_sha256"] = strings.Repeat("0", 64)
	if _, err := executor.backupChunk(context.Background(), job, transferTestProgress); err == nil {
		t.Fatal("incorrect backup checksum was accepted")
	}
}

func TestReadVerifiedChunkEnforcesOneMiBLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readVerifiedChunk(path, 0, maxTransferChunk+1, "", 0); err == nil {
		t.Fatal("oversized chunk was accepted")
	}
}

func TestLineSinkBoundsUnterminatedLines(t *testing.T) {
	var destination bytes.Buffer
	sink := &lineSink{stream: "stdout", log: &destination}
	input := bytes.Repeat([]byte("x"), maxBufferedLogLine*2)
	if n, err := sink.Write(input); err != nil || n != len(input) {
		t.Fatalf("write = %d, %v", n, err)
	}
	sink.Flush()
	if sink.buf.Len() != 0 || !strings.Contains(destination.String(), "…[continued]") {
		t.Fatalf("unterminated line was not bounded: buffered=%d output=%d", sink.buf.Len(), destination.Len())
	}
}
