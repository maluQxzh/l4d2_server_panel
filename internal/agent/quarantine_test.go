package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPurgeQuarantineRequiresSevenDayRetentionAndRemovesOnlyExactEntry(t *testing.T) {
	root := t.TempDir()
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	oldName := "instance-a-" + now.Add(-8*24*time.Hour).Format("20060102T150405Z")
	freshName := "instance-b-" + now.Add(-6*24*time.Hour).Format("20060102T150405Z")
	for _, name := range []string{oldName, freshName, "operator-owned"} {
		if err := os.MkdirAll(filepath.Join(root, ".quarantine", name), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	if removed, err := purgeQuarantineAt(guard, 0, freshName, now); err == nil || removed || !strings.Contains(err.Error(), "seven-day") {
		t.Fatalf("fresh purge = %v, %v; want minimum-retention rejection", removed, err)
	}
	removed, err := purgeQuarantineAt(guard, 0, oldName, now)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("eligible quarantine entry was not removed")
	}
	if _, err := os.Stat(filepath.Join(root, ".quarantine", oldName)); !os.IsNotExist(err) {
		t.Fatalf("purged entry still exists: %v", err)
	}
	for _, name := range []string{freshName, "operator-owned"} {
		if _, err := os.Stat(filepath.Join(root, ".quarantine", name)); err != nil {
			t.Fatalf("entry %q should remain: %v", name, err)
		}
	}
}

func TestPurgeQuarantineRejectsTraversalAndInvalidRoot(t *testing.T) {
	guard, err := NewPathGuard([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldStamp := now.Add(-8 * 24 * time.Hour).Format("20060102T150405Z")
	for _, name := range []string{"../escape-" + oldStamp, `..\escape-` + oldStamp, "operator-owned"} {
		if _, err := purgeQuarantineAt(guard, 0, name, now); err == nil {
			t.Fatalf("unsafe quarantine entry %q was accepted", name)
		}
	}
	if _, err := purgeQuarantineAt(guard, 4, "instance-"+oldStamp, now); err == nil {
		t.Fatal("invalid root index was accepted")
	}
}

func TestPurgeQuarantineRejectsLinkedTree(t *testing.T) {
	root := t.TempDir()
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	name := "instance-a-" + now.Add(-8*24*time.Hour).Format("20060102T150405Z")
	target := filepath.Join(root, ".quarantine", name)
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(target, "linked-data")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if removed, err := purgeQuarantineAt(guard, 0, name, now); err == nil || removed {
		t.Fatalf("linked tree purge = %v, %v; want rejection", removed, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("rejected quarantine entry was modified: %v", err)
	}
}

func TestPrepareQuarantineTargetRejectsLinkedQuarantineRoot(t *testing.T) {
	root := t.TempDir()
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".quarantine")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	name := "instance-a-" + time.Now().UTC().Format("20060102T150405Z")
	if _, err := PrepareQuarantineTarget(guard, 0, name); err == nil {
		t.Fatal("linked quarantine root was accepted")
	}
}
