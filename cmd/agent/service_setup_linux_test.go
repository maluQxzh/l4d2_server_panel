//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/agent"
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

func TestRegisteredServiceGamePathsUsesExactPersistedInstallations(t *testing.T) {
	data := t.TempDir()
	installation := filepath.Join(t.TempDir(), "game", "l4d2")
	if err := os.MkdirAll(installation, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installation, "srcds_run"), []byte("srcds"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"installations": map[string]any{"one": map[string]any{"path": installation}}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "runtime-environment.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := registeredServiceGamePaths(&agent.Config{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != installation {
		t.Fatalf("registered paths = %#v, want %q", paths, installation)
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

func TestChownTreeRejectsHardLinksBeforeOwnershipChanges(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := chownTreeWithoutLinks(root, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("hard-linked file tree was accepted for recursive service ownership")
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
