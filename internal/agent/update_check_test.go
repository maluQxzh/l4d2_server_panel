package agent

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type fakeSteamOperations struct {
	publicBuild    string
	installedBuild string
	installedFrom  string
	installedErr   error
}

func (fakeSteamOperations) Install(context.Context, string, int, bool, func(int, string)) error {
	return nil
}

func (fakeSteamOperations) WorkshopItem(context.Context, string, func(int, string)) ([]string, error) {
	return nil, nil
}

func (steam fakeSteamOperations) PublicBuildID(context.Context, int) (string, error) {
	return steam.publicBuild, nil
}

func (steam fakeSteamOperations) InstalledBuildID(string, int) (string, string, error) {
	return steam.installedBuild, steam.installedFrom, steam.installedErr
}

func TestExecutorCheckUpdateComparesBuilds(t *testing.T) {
	root := t.TempDir()
	paths, err := NewPathGuard([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "instance-1", RootIndex: 0, RelativePath: "instance-1"}); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{
		paths: paths, registry: registry,
		steam:  fakeSteamOperations{publicBuild: "200", installedBuild: "100", installedFrom: "instance"},
		logger: log.New(io.Discard, "", 0), locks: newKeyedLocker(),
	}
	result, err := executor.Execute(context.Background(), domain.Job{ID: "job-1", InstanceID: "instance-1", Type: "check_update"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if available, _ := result["available"].(bool); !available {
		t.Fatalf("expected an available update: %#v", result)
	}
	if result["installed_build"] != "100" || result["available_build"] != "200" {
		t.Fatalf("unexpected update result: %#v", result)
	}
}

func TestCheckUpdateSharesTheInstanceMutationLock(t *testing.T) {
	if !agentJobRequiresInstanceLock("check_update", nil) {
		t.Fatal("check_update could run concurrently with an instance update")
	}
	if !agentJobRequiresInstanceLock("update", nil) || !agentJobRequiresInstanceLock("file", map[string]any{"operation": "write"}) {
		t.Fatal("mutating operations did not require the instance lock")
	}
	for _, action := range []string{"a2s.info", "a2s.players", "rcon.players", "logs.search"} {
		if agentJobRequiresInstanceLock(action, nil) {
			t.Fatalf("live read operation %q unexpectedly took the mutation lock", action)
		}
	}
	if agentJobRequiresInstanceLock("file", map[string]any{"operation": "read"}) {
		t.Fatal("bounded file read unexpectedly took the mutation lock")
	}
}

func TestNormalizeCheckUpdateJobType(t *testing.T) {
	for _, value := range []string{"check_update", "check.update", "update.check"} {
		if normalized := normalizeJobType(value); normalized != "check_update" {
			t.Fatalf("normalize %q = %q", value, normalized)
		}
	}
}
