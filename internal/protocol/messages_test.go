package protocol

import (
	"encoding/json"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

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
