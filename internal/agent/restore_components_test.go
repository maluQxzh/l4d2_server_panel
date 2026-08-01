package agent

import (
	"errors"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestCoreComponentsForNativeFilesUsesDependencyOrder(t *testing.T) {
	files := []string{
		"left4dead2/addons/sourcemod/bin/sourcemod.2.l4d2.so",
		"left4dead2/addons/metamod/bin/server.dll",
		"left4dead2/addons/sourcemod/extensions/third-party.so",
	}
	if got, want := coreComponentsForNativeFiles(files), []string{"metamod", "sourcemod"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("components = %#v, want %#v", got, want)
	}
}

func TestRestoreFailureRetainsCauseWhenCheckpointRecoveryFails(t *testing.T) {
	t.Parallel()
	// A missing registry record makes the recovery itself fail. This still
	// verifies that callers retain both the original stage and cause instead of
	// replacing a useful restore error with an opaque rollback failure.
	dataDir := t.TempDir()
	registry, err := OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	processes, err := NewProcessManager(dataDir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{registry: registry, processes: processes}
	err = executor.restoreFailure(domain.Job{ID: "job-1", InstanceID: "missing"}, "missing.zip", "copy restored files", errors.New("disk full"))
	if err == nil || !strings.Contains(err.Error(), "copy restored files failed") || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "rollback also failed") {
		t.Fatalf("unexpected restore failure: %v", err)
	}
}
