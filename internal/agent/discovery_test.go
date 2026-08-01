package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func createDiscoveredInstallation(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "left4dead2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "srcds.exe"), []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "left4dead2", "gameinfo.txt"), []byte("game"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "appmanifest_222860.acf"), []byte(`"appid" "222860"`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverInstallationsFindsOnlyConfiguredRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "instances")
	installation := filepath.Join(root, "existing-server")
	outside := filepath.Join(base, "outside-server")
	createDiscoveredInstallation(t, installation)
	createDiscoveredInstallation(t, outside)
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "known", RootIndex: 0, RelativePath: "existing-server"}); err != nil {
		t.Fatal(err)
	}
	result, err := DiscoverInstallations(context.Background(), guard, registry, 4, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %#v, want one", result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.Path != "existing-server" || candidate.RootIndex != 0 || !candidate.Managed || !candidate.HasGameInfo || !candidate.HasAppManifest {
		t.Fatalf("unexpected candidate: %#v", candidate)
	}
}

func TestDiscoverInstallationsSkipsSymlinkOrReparsePoint(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "instances")
	outside := filepath.Join(base, "outside-server")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	createDiscoveredInstallation(t, outside)
	if err := os.Symlink(outside, filepath.Join(root, "linked-server")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := DiscoverInstallations(context.Background(), guard, nil, 4, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 || result.SkippedLinks != 1 {
		t.Fatalf("result = %#v, want no candidates and one skipped link", result)
	}
}

func TestDiscoverInstallationsHonorsEntryLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instances")
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	guard, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := DiscoverInstallations(context.Background(), guard, nil, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.ScannedDirectories > 3 {
		t.Fatalf("result = %#v, want a bounded scan of root plus two entries", result)
	}
}

func TestInspectInstallationCandidateMarksMatchingProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instances")
	installation := filepath.Join(root, "running")
	createDiscoveredInstallation(t, installation)
	process := discoveredProcess{PID: 4242, Paths: []string{canonicalProcessPath(filepath.Join(installation, "srcds.exe"))}}
	candidate, ok := inspectInstallationCandidate(root, 0, "running", installation, map[string]struct{}{}, []discoveredProcess{process})
	if !ok || !candidate.LikelyRunning || candidate.PID != 4242 {
		t.Fatalf("candidate = %#v, ok = %v", candidate, ok)
	}
}
