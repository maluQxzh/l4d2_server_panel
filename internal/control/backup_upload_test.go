package control

import (
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestValidateBackupUploadMetadata(t *testing.T) {
	sha := strings.Repeat("a", 64)
	if err := validateBackupUploadMetadata("portable.zip", "linux/amd64", 1024, sha); err != nil {
		t.Fatal(err)
	}
	invalid := []struct {
		name     string
		platform string
		size     int64
		sha      string
	}{
		{"../escape.zip", "linux/amd64", 1, sha},
		{"portable.tar", "linux/amd64", 1, sha},
		{"portable.zip", "darwin/amd64", 1, sha},
		{"portable.zip", "windows/amd64", 0, sha},
		{"portable.zip", "windows/amd64", maxBackupUploadSize + 1, sha},
		{"portable.zip", "windows/amd64", 1, ""},
		{"portable.zip", "windows/amd64", 1, "not-a-sha"},
	}
	for _, item := range invalid {
		if err := validateBackupUploadMetadata(item.name, item.platform, item.size, item.sha); err == nil {
			t.Fatalf("invalid upload metadata was accepted: %#v", item)
		}
	}
}

func TestBackupUploadChunkSecretRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{"upload_id": "upload", "content_base64": "cG9ydGFibGU="}
	if err := sealJobPayloadSecrets(key, "backup.upload.chunk", payload); err != nil {
		t.Fatal(err)
	}
	if payload["content_base64"] != encryptedValuePlaceholder || stringFromAny(payload["content_base64_ciphertext"]) == "" {
		t.Fatalf("chunk body was not encrypted: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "backup.upload.chunk", payload); err != nil {
		t.Fatal(err)
	}
	if payload["content_base64"] != "cG9ydGFibGU=" || payload["content_base64_ciphertext"] != nil {
		t.Fatalf("chunk body did not round trip: %#v", payload)
	}
}

func TestSameBackupUploadChunkRequiresMatchingIdempotentMetadata(t *testing.T) {
	payload := map[string]any{"upload_id": "upload-1", "offset": int64(4), "length": 8, "chunk_sha256": strings.Repeat("b", 64)}
	job := domain.Job{Type: "backup.upload.chunk", InstanceID: "instance-1", Payload: cloneMap(payload)}
	if !sameBackupUploadChunk(job, "instance-1", "upload-1", payload) {
		t.Fatal("matching chunk replay was rejected")
	}
	changed := cloneMap(payload)
	changed["offset"] = int64(5)
	if sameBackupUploadChunk(job, "instance-1", "upload-1", changed) {
		t.Fatal("idempotency key reuse with a changed offset was accepted")
	}
}
