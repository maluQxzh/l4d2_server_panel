package control

import (
	"testing"
	"time"
)

func TestParseQuarantineEntryTimeAcceptsOnlyAgentBasenames(t *testing.T) {
	want := time.Date(2026, 7, 24, 12, 34, 56, 0, time.UTC)
	got, err := parseQuarantineEntryTime("8edc040e-050d-4cd9-b466-854922b28598-20260724T123456Z")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("timestamp = %s, want %s", got, want)
	}
	for _, value := range []string{
		"../instance-20260724T123456Z",
		`..\instance-20260724T123456Z`,
		"instance-20260724T123456Z/child",
		"instance-20260724T123456Z ",
		"operator-owned",
	} {
		if _, err := parseQuarantineEntryTime(value); err == nil {
			t.Errorf("unsafe entry %q was accepted", value)
		}
	}
}

func TestPurgeConfirmationTTLIsShorterThanRetention(t *testing.T) {
	if purgeConfirmationTokenTTL <= 0 || purgeConfirmationTokenTTL > 10*time.Minute {
		t.Fatalf("purge confirmation TTL = %s, want at most 10 minutes", purgeConfirmationTokenTTL)
	}
	if quarantineRetention != 7*24*time.Hour {
		t.Fatalf("quarantine retention = %s, want seven days", quarantineRetention)
	}
}

func TestQuarantineRootIndexMustBeAnExactNonNegativeInteger(t *testing.T) {
	for _, value := range []any{0, int64(4), float64(7)} {
		if _, ok := nonNegativeQuarantineRootIndex(value); !ok {
			t.Errorf("valid root index %#v was rejected", value)
		}
	}
	for _, value := range []any{nil, -1, float64(1.5), "0"} {
		if _, ok := nonNegativeQuarantineRootIndex(value); ok {
			t.Errorf("invalid root index %#v was accepted", value)
		}
	}
}
