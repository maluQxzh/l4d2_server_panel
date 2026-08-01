package control

import (
	"reflect"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestParseFiveFieldCronRejectsExtendedForms(t *testing.T) {
	t.Parallel()
	if _, err := parseFiveFieldCron("0 4 * * *"); err != nil {
		t.Fatalf("valid five-field cron rejected: %v", err)
	}
	for _, expression := range []string{
		"@every 1h",
		"@daily",
		"CRON_TZ=UTC 0 4 * * *",
		"TZ=Asia/Hong_Kong 0 4 * * *",
		"0 0 4 * * *",
	} {
		expression := expression
		t.Run(expression, func(t *testing.T) {
			t.Parallel()
			if _, err := parseFiveFieldCron(expression); err == nil {
				t.Fatalf("extended cron form was accepted: %q", expression)
			}
		})
	}
}

func TestMaintenanceWindowOccurrenceHongKongAndCatchUpBoundary(t *testing.T) {
	t.Parallel()
	location, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	scheduledLocal := time.Date(2026, time.August, 2, 4, 0, 0, 0, location)
	wantUTC := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)

	for name, now := range map[string]time.Time{
		"at occurrence":       scheduledLocal,
		"inside five minutes": scheduledLocal.Add(4*time.Minute + 59*time.Second),
		"inclusive boundary":  scheduledLocal.Add(maintenanceCatchUpWindow),
	} {
		name, now := name, now
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, due, occurrenceErr := maintenanceWindowOccurrence("0 4 * * *", location, now)
			if occurrenceErr != nil {
				t.Fatal(occurrenceErr)
			}
			if !due || !got.Equal(wantUTC) {
				t.Fatalf("occurrence at %s = %s, due=%v; want %s, true", now, got, due, wantUTC)
			}
		})
	}

	outside := scheduledLocal.Add(maintenanceCatchUpWindow + time.Nanosecond)
	if got, due, occurrenceErr := maintenanceWindowOccurrence("0 4 * * *", location, outside); occurrenceErr != nil {
		t.Fatal(occurrenceErr)
	} else if due || !got.IsZero() {
		t.Fatalf("out-of-window occurrence = %s, due=%v; want zero, false", got, due)
	}
}

func TestMaintenanceUpdateKeyRoundTripAndRejectsMalformedValues(t *testing.T) {
	t.Parallel()
	instanceID := "123e4567-e89b-12d3-a456-426614174000"
	windowHKT := time.Date(2026, time.August, 2, 4, 0, 0, 0, time.FixedZone("HKT", 8*60*60))
	windowUTC := windowHKT.UTC()
	key := maintenanceUpdateKey(windowHKT, instanceID)
	if same := maintenanceUpdateKey(windowUTC, instanceID); same != key {
		t.Fatalf("equivalent instants produced different keys: %q and %q", key, same)
	}
	parsedWindow, parsedInstanceID, ok := parseMaintenanceUpdateKey(key)
	if !ok || !parsedWindow.Equal(windowUTC) || parsedInstanceID != instanceID {
		t.Fatalf("parsed key = (%s, %q, %v), want (%s, %q, true)", parsedWindow, parsedInstanceID, ok, windowUTC, instanceID)
	}

	for _, malformed := range []string{
		"",
		"schedule:update:" + instanceID,
		maintenanceUpdateKeyPrefix + "missing-timestamp:" + instanceID,
		maintenanceUpdateKeyPrefix + "-1:" + instanceID,
		maintenanceUpdateKeyPrefix + "0:not-a-uuid",
		maintenanceUpdateKeyPrefix + "0:123e4567e89b12d3a456426614174000",
		maintenanceUpdateKeyPrefix + "0:" + instanceID + ":extra",
	} {
		if parsedAt, parsedID, valid := parseMaintenanceUpdateKey(malformed); valid || !parsedAt.IsZero() || parsedID != "" {
			t.Fatalf("malformed key %q parsed as (%s, %q, %v)", malformed, parsedAt, parsedID, valid)
		}
	}
}

func TestMaintenanceBatchBlockedOnlyByOtherOrMalformedWindows(t *testing.T) {
	t.Parallel()
	window := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	firstID := "123e4567-e89b-12d3-a456-426614174000"
	secondID := "123e4567-e89b-12d3-a456-426614174001"
	if maintenanceBatchBlocked(nil, window) {
		t.Fatal("empty active batch unexpectedly blocked a window")
	}
	if maintenanceBatchBlocked([]string{
		maintenanceUpdateKey(window, firstID),
		maintenanceUpdateKey(window, secondID),
	}, window) {
		t.Fatal("active jobs from the same window unexpectedly blocked their batch replay")
	}
	if !maintenanceBatchBlocked([]string{maintenanceUpdateKey(window.Add(-24*time.Hour), firstID)}, window) {
		t.Fatal("an older active maintenance window did not block a new batch")
	}
	if !maintenanceBatchBlocked([]string{maintenanceUpdateKey(window, firstID), "maintenance:update:malformed"}, window) {
		t.Fatal("a malformed active maintenance key did not fail closed")
	}
}

func TestAutomaticMaintenancePayloadCopiesWarningsAndKeepsSnapshotState(t *testing.T) {
	t.Parallel()
	instance := domain.Instance{
		ID: "123e4567-e89b-12d3-a456-426614174000", NodeID: "123e4567-e89b-12d3-a456-426614174001",
		Name: "running-server", Status: domain.InstanceRunning, Port: 27015,
		Config: domain.InstanceConfig{Hostname: "running-server", Mode: "coop", MaxPlayers: 8},
	}
	first := automaticMaintenancePayload(instance)
	if maintenance, ok := first["maintenance"].(bool); !ok || !maintenance {
		t.Fatalf("maintenance marker = %#v, want true", first["maintenance"])
	}
	snapshot, ok := first["instance"].(domain.Instance)
	if !ok || snapshot.Status != domain.InstanceRunning || !reflect.DeepEqual(snapshot, instance) {
		t.Fatalf("payload snapshot = %#v, want running instance %#v", first["instance"], instance)
	}
	warnings, ok := first["warning_seconds"].([]int)
	wantWarnings := []int{600, 300, 60}
	if !ok || !reflect.DeepEqual(warnings, wantWarnings) {
		t.Fatalf("warning_seconds = %#v, want %#v", first["warning_seconds"], wantWarnings)
	}

	warnings[0] = 1
	secondWarnings, ok := automaticMaintenancePayload(instance)["warning_seconds"].([]int)
	if !ok || !reflect.DeepEqual(secondWarnings, wantWarnings) {
		t.Fatalf("a caller mutation polluted a later payload: %#v", secondWarnings)
	}
	if !reflect.DeepEqual(automaticMaintenanceWarningSeconds, wantWarnings) {
		t.Fatalf("a caller mutation polluted scheduler defaults: %#v", automaticMaintenanceWarningSeconds)
	}
}
