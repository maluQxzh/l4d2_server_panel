package control

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestAgentCapabilityGatesUpdateProbe(t *testing.T) {
	hub := NewAgentHub(nil, nil, slog.Default(), nil)
	hub.conns["new"] = &agentConnection{capabilities: map[string]struct{}{"check_update": {}}}
	hub.conns["old"] = &agentConnection{capabilities: map[string]struct{}{}}
	if !hub.Supports("new", "check_update") {
		t.Fatal("expected advertised update capability")
	}
	if hub.Supports("old", "check_update") || hub.Supports("offline", "check_update") {
		t.Fatal("must not queue update checks for unsupported or offline Agents")
	}
}

func TestUpdateProbeKeyUsesThirtyMinuteWindow(t *testing.T) {
	first := time.Date(2026, time.August, 1, 10, 1, 0, 0, time.FixedZone("HKT", 8*60*60))
	sameWindow := first.Add(28 * time.Minute)
	nextWindow := first.Add(29 * time.Minute)
	firstKey := updateProbeKey("instance-1", first)
	if sameKey := updateProbeKey("instance-1", sameWindow); sameKey != firstKey {
		t.Fatalf("same half-hour window generated different keys: %q and %q", firstKey, sameKey)
	}
	if nextKey := updateProbeKey("instance-1", nextWindow); nextKey == firstKey {
		t.Fatalf("next half-hour window reused key %q", firstKey)
	}
	if !strings.HasPrefix(firstKey, "monitor:update:instance-1:") {
		t.Fatalf("unexpected update key %q", firstKey)
	}
}
