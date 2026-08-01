package agent

import (
	"testing"
	"time"
)

func TestCachedPlayersExpiresStaleA2SSample(t *testing.T) {
	now := time.Now().UTC()
	executor := &Executor{telemetry: map[string]instanceTelemetry{
		"fresh": {players: 4, observedAt: now.Add(-time.Minute)},
		"stale": {players: 8, observedAt: now.Add(-3 * time.Minute)},
	}}
	if got := executor.cachedPlayers("fresh", now); got != 4 {
		t.Fatalf("fresh players = %d", got)
	}
	if got := executor.cachedPlayers("stale", now); got != 0 {
		t.Fatalf("stale players = %d", got)
	}
}
