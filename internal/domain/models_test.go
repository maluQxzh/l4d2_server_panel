package domain

import (
	"encoding/json"
	"testing"
)

func TestInstanceConfigDoesNotExposeEmptyPassword(t *testing.T) {
	raw, err := json.Marshal(InstanceConfig{Hostname: "测试服", Mode: "coop"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Fatal("empty json")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["server_password"]; ok {
		t.Fatalf("empty password should be omitted: %s", raw)
	}
}

func TestStableJobStatuses(t *testing.T) {
	want := []JobStatus{JobQueued, JobDispatched, JobRunning, JobSucceeded, JobFailed, JobCanceled}
	seen := map[JobStatus]bool{}
	for _, status := range want {
		if status == "" || seen[status] {
			t.Fatalf("invalid or duplicate status %q", status)
		}
		seen[status] = true
	}
}
