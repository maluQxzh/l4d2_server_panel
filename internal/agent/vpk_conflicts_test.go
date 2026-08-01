package agent

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateVPKContentConflictsRejectsVirtualPathOverlap(t *testing.T) {
	instancePath := t.TempDir()
	addons := filepath.Join(instancePath, "left4dead2", "addons")
	if err := os.MkdirAll(addons, 0o750); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(addons, "existing.vpk")
	candidate := filepath.Join(t.TempDir(), "candidate.vpk")
	writeConflictTestVPK(t, existing, "scripts", "shared", "txt", []byte("old"))
	writeConflictTestVPK(t, candidate, "scripts", "shared", "txt", []byte("new"))

	err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{{
		Source: candidate, TargetRelative: "left4dead2/addons/candidate.vpk",
	}})
	if err == nil || !strings.Contains(err.Error(), "scripts/shared.txt") || !strings.Contains(err.Error(), "existing.vpk") {
		t.Fatalf("content conflict = %v; want bounded path and owner", err)
	}
}

func TestValidateVPKContentConflictsAllowsExactPackageReplacement(t *testing.T) {
	instancePath := t.TempDir()
	addons := filepath.Join(instancePath, "left4dead2", "addons")
	if err := os.MkdirAll(addons, 0o750); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(addons, "same.vpk")
	candidate := filepath.Join(t.TempDir(), "same.vpk")
	writeConflictTestVPK(t, existing, "scripts", "shared", "txt", []byte("old"))
	writeConflictTestVPK(t, candidate, "scripts", "shared", "txt", []byte("new"))

	if err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{{
		Source: candidate, TargetRelative: "left4dead2/addons/same.vpk",
	}}); err != nil {
		t.Fatalf("exact replacement was rejected: %v", err)
	}
}

func TestValidateVPKContentConflictsChecksWorkshopBatch(t *testing.T) {
	instancePath := t.TempDir()
	first := filepath.Join(t.TempDir(), "first.vpk")
	second := filepath.Join(t.TempDir(), "second.vpk")
	writeConflictTestVPK(t, first, "materials/panel", "shared", "vmt", []byte("one"))
	writeConflictTestVPK(t, second, "materials/panel", "shared", "vmt", []byte("two"))

	err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{
		{Source: first, TargetRelative: "left4dead2/addons/1_first.vpk"},
		{Source: second, TargetRelative: "left4dead2/addons/2_second.vpk"},
	})
	if err == nil || !strings.Contains(err.Error(), "materials/panel/shared.vmt") || !strings.Contains(err.Error(), "1_first.vpk") {
		t.Fatalf("batch content conflict = %v", err)
	}
}

func TestValidateVPKContentConflictsIgnoresDisabledPackages(t *testing.T) {
	instancePath := t.TempDir()
	disabled := filepath.Join(instancePath, "left4dead2", "addons", "panel-disabled", "disabled.vpk")
	if err := os.MkdirAll(filepath.Dir(disabled), 0o750); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "candidate.vpk")
	writeConflictTestVPK(t, disabled, "scripts", "shared", "txt", []byte("old"))
	writeConflictTestVPK(t, candidate, "scripts", "shared", "txt", []byte("new"))

	if err := validateVPKContentConflicts(instancePath, []vpkInstallCandidate{{
		Source: candidate, TargetRelative: "left4dead2/addons/candidate.vpk",
	}}); err != nil {
		t.Fatalf("disabled package caused a conflict: %v", err)
	}
}

func writeConflictTestVPK(t *testing.T, path, directory, name, extension string, data []byte) {
	t.Helper()
	tree := minimalVPKTree(directory, name, extension, data, 0xffff)
	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[:4], vpkSignature)
	binary.LittleEndian.PutUint32(header[4:8], 1)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(tree)))
	if err := os.WriteFile(path, append(append(header, tree...), data...), 0o600); err != nil {
		t.Fatal(err)
	}
}
