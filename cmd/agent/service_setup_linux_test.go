//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeManagedServicePathRejectsBroadAndExecutableParents(t *testing.T) {
	if _, err := safeManagedServicePath(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root was accepted as an Agent-managed service path")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := safeManagedServicePath(filepath.Dir(executable)); err == nil {
		t.Fatal("directory containing the Agent executable was accepted for recursive ownership changes")
	}
	candidate := filepath.Join(t.TempDir(), "agent", "data")
	if got, err := safeManagedServicePath(candidate); err != nil || got != candidate {
		t.Fatalf("safe nested path = %q, %v", got, err)
	}
}

func TestPathWithinAny(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "agent-data")
	parents := map[string]bool{parent: true}
	if !pathWithinAny(filepath.Join(parent, "instances"), parents) {
		t.Fatal("nested path was not recognized")
	}
	if pathWithinAny(filepath.Join(filepath.Dir(parent), "other"), parents) {
		t.Fatal("sibling path was treated as nested")
	}
}

func TestLinePresentMatchesWholeSystemdDirective(t *testing.T) {
	unit := "[Service]\nUser=l4d2panel\nExecStart=/opt/agent \"run\"\n"
	if !linePresent(unit, "User=l4d2panel") {
		t.Fatal("exact systemd directive was not found")
	}
	if linePresent(unit, "User=root") || linePresent(unit, "User=l4d2") {
		t.Fatal("partial or different systemd directive was accepted")
	}
}
