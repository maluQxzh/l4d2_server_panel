package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestPackageUploadWireLimits(t *testing.T) {
	if MaxPackageUploadBytes != 100<<20 {
		t.Fatalf("package upload limit = %d, want 100 MiB", MaxPackageUploadBytes)
	}
	if MaxEncodedPackageUploadBytes != base64.StdEncoding.EncodedLen(MaxPackageUploadBytes) {
		t.Fatalf("encoded package limit = %d, want %d", MaxEncodedPackageUploadBytes, base64.StdEncoding.EncodedLen(MaxPackageUploadBytes))
	}
	if MaxPackageUploadEnvelopeBytes <= MaxEncodedPackageUploadBytes {
		t.Fatal("package envelope limit does not leave room for JSON metadata")
	}
}

func TestNewEnvelope(t *testing.T) {
	want := JobUpdate{Status: domain.JobRunning, Progress: 42, Message: "正在更新"}
	env, err := NewEnvelope(TypeJobUpdate, "node-1", "job-1", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error = %v", err)
	}
	if env.Version != Version || env.NodeID != "node-1" || env.JobID != "job-1" {
		t.Fatalf("unexpected envelope: %#v", env)
	}
	var got JobUpdate
	if err := json.Unmarshal(env.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if got.Status != want.Status || got.Progress != want.Progress || got.Message != want.Message {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
}

func TestAdmissionRetryIsOmittedUnlessNegotiated(t *testing.T) {
	legacy, err := json.Marshal(JobCommand{Job: domain.Job{ID: "job"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "admission_retry") {
		t.Fatal("old command received new field")
	}
	current, err := json.Marshal(JobCommand{Job: domain.Job{ID: "job"}, AdmissionRetry: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), `"admission_retry":true`) {
		t.Fatal("negotiated command lost retry flag")
	}
}

func TestCompatibleVersionWindow(t *testing.T) {
	for _, value := range []string{Version, PreviousVersion} {
		if !CompatibleVersion(value) {
			t.Fatalf("expected %q to be compatible", value)
		}
	}
	for _, value := range []string{"", "0.8", "1.1", "2.0"} {
		if CompatibleVersion(value) {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestTruncateJobUpdateMessagePreservesWireLimitAndUTF8(t *testing.T) {
	tests := []string{
		strings.Repeat("x", MaxJobUpdateMessageBytes+1),
		strings.Repeat("x", MaxJobUpdateMessageBytes-1) + "界",
		strings.Repeat("x", MaxJobUpdateMessageBytes-1) + string([]byte{0xff}),
	}
	for _, input := range tests {
		got := TruncateJobUpdateMessage(input)
		if len(got) > MaxJobUpdateMessageBytes {
			t.Fatalf("truncated message has %d bytes", len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatal("truncated message is not valid UTF-8")
		}
		raw, err := json.Marshal(JobUpdate{Message: got})
		if err != nil {
			t.Fatal(err)
		}
		var update JobUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			t.Fatal(err)
		}
		if len(update.Message) > MaxJobUpdateMessageBytes {
			t.Fatalf("JSON round trip expanded message to %d bytes", len(update.Message))
		}
	}
}
