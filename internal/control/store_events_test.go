package control

import (
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestCompactPersistentEventDataDropsJobPayloadAndResult(t *testing.T) {
	job := domain.Job{ID: "job", NodeID: "node", InstanceID: "instance", Type: "file", Status: domain.JobQueued,
		Payload: map[string]any{"content_base64_ciphertext": strings.Repeat("x", 1024)}, Result: map[string]any{"secret": true}}
	compact := compactPersistentEventData(map[string]any{"job": job})
	stored, ok := compact["job"].(map[string]any)
	if !ok {
		t.Fatalf("compact job = %#v", compact["job"])
	}
	if stored["id"] != "job" || stored["status"] != domain.JobQueued {
		t.Fatalf("job summary lost safe fields: %#v", stored)
	}
	if stored["payload"] != nil || stored["result"] != nil {
		t.Fatalf("job payload or result survived compaction: %#v", stored)
	}
}
