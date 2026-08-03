package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpenRegistryMigratesLegacyDesiredState(t *testing.T) {
	dataDir := t.TempDir()
	legacy := map[string]any{
		"running-before-upgrade": map[string]any{
			"id": "running-before-upgrade", "auto_restart": true,
		},
		"stopped-before-upgrade": map[string]any{
			"id": "stopped-before-upgrade", "auto_restart": false,
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "instances.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	running, _ := registry.Get("running-before-upgrade")
	stopped, _ := registry.Get("stopped-before-upgrade")
	if !running.DesiredStateKnown || !running.DesiredRunning {
		t.Fatalf("legacy running record was not migrated: %#v", running)
	}
	if !stopped.DesiredStateKnown || stopped.DesiredRunning {
		t.Fatalf("legacy stopped record was not migrated: %#v", stopped)
	}
	if _, err := OpenRegistry(dataDir); err != nil {
		t.Fatalf("migrated registry was not durable: %v", err)
	}
}

func TestOpenRegistryRemovesLegacyRelativeInstancePaths(t *testing.T) {
	dataDir := t.TempDir()
	records := map[string]InstanceRecord{
		"legacy-relative": {ID: "legacy-relative", RelativePath: "servers/legacy"},
		"absolute":        {ID: "absolute", RelativePath: filepath.Join(t.TempDir(), "absolute")},
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "instances.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("legacy-relative"); ok {
		t.Fatal("legacy relative instance registry record survived upgrade")
	}
	if _, ok := registry.Get("absolute"); !ok {
		t.Fatal("absolute instance registry record was removed")
	}
}

func TestRegistryPutAtomicallyRejectsConcurrentDuplicatePaths(t *testing.T) {
	registry, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 16
	start := make(chan struct{})
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	for i := 0; i < contenders; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			relative := "instances/shared"
			if index%2 == 0 {
				relative = "instances/staging/../shared"
			}
			results <- registry.Put(InstanceRecord{
				ID:           fmt.Sprintf("instance-%02d", index),
				RootIndex:    1,
				RelativePath: relative,
			})
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)

	succeeded := 0
	conflicted := 0
	for putErr := range results {
		switch {
		case putErr == nil:
			succeeded++
		case errors.Is(putErr, ErrInstancePathRegistered):
			conflicted++
		default:
			t.Fatalf("unexpected Put error: %v", putErr)
		}
	}
	if succeeded != 1 || conflicted != contenders-1 {
		t.Fatalf("succeeded=%d conflicted=%d, want 1 and %d", succeeded, conflicted, contenders-1)
	}
	if records := registry.List(); len(records) != 1 {
		t.Fatalf("registry contains %d records, want 1", len(records))
	}
	if _, err := OpenRegistry(filepath.Dir(registry.path)); err != nil {
		t.Fatalf("winning record was not persisted cleanly: %v", err)
	}
}

func TestRegistryPutAllowsSameIDUpdateAndReleasesOldPath(t *testing.T) {
	registry, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "alpha", Name: "old", RootIndex: 0, RelativePath: "servers/shared"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: "alpha", Name: "new", RootIndex: 0, RelativePath: "servers/moved"}); err != nil {
		t.Fatalf("same-ID update failed: %v", err)
	}
	if err := registry.Put(InstanceRecord{ID: "bravo", RootIndex: 0, RelativePath: "servers/shared"}); err != nil {
		t.Fatalf("old path remained reserved after same-ID update: %v", err)
	}
	if err := registry.Put(InstanceRecord{ID: "charlie", RootIndex: 0, RelativePath: "servers/./moved"}); !errors.Is(err, ErrInstancePathRegistered) {
		t.Fatalf("duplicate of updated path error = %v, want ErrInstancePathRegistered", err)
	}
	updated, ok := registry.Get("alpha")
	if !ok || updated.Name != "new" || updated.RelativePath != "servers/moved" {
		t.Fatalf("same-ID update was not retained: %#v, ok=%v", updated, ok)
	}
}

func TestSameInstancePathForOS(t *testing.T) {
	tests := []struct {
		name      string
		rootA     int
		relativeA string
		rootB     int
		relativeB string
		goos      string
		want      bool
	}{
		{
			name: "windows normalizes separators dots and case", rootA: 0, relativeA: `Servers\L4D2\staging\..\Alpha`,
			rootB: 0, relativeB: "servers/l4d2/alpha", goos: "windows", want: true,
		},
		{
			name: "linux normalizes slash dot segments", rootA: 0, relativeA: "Servers/L4D2/staging/../Alpha",
			rootB: 0, relativeB: "Servers/L4D2/Alpha", goos: "linux", want: true,
		},
		{
			name: "linux remains case sensitive", rootA: 0, relativeA: "servers/Alpha",
			rootB: 0, relativeB: "servers/alpha", goos: "linux", want: false,
		},
		{
			name: "linux treats backslash as a filename character", rootA: 0, relativeA: `servers\alpha`,
			rootB: 0, relativeB: "servers/alpha", goos: "linux", want: false,
		},
		{
			name: "different roots never conflict", rootA: 0, relativeA: "servers/alpha",
			rootB: 1, relativeB: "servers/alpha", goos: "windows", want: false,
		},
		{
			name: "absolute Windows paths ignore legacy root indexes", rootA: 0, relativeA: `D:\Servers\Alpha`,
			rootB: 9, relativeB: "d:/servers/alpha", goos: "windows", want: true,
		},
		{
			name: "absolute Linux paths ignore legacy root indexes", rootA: 0, relativeA: "/srv/l4d2/alpha",
			rootB: 9, relativeB: "/srv/l4d2/alpha", goos: "linux", want: true,
		},
		{
			name: "legacy blank paths are not registered directories", rootA: 0, relativeA: "",
			rootB: 0, relativeB: "", goos: "windows", want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameInstancePathForOS(tt.rootA, tt.relativeA, tt.rootB, tt.relativeB, tt.goos); got != tt.want {
				t.Fatalf("sameInstancePathForOS(...) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenRegistryRejectsPersistedDuplicatePaths(t *testing.T) {
	dataDir := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "servers", "shared")
	records := map[string]InstanceRecord{
		"alpha": {ID: "alpha", RootIndex: 0, RelativePath: absolute},
		"bravo": {ID: "bravo", RootIndex: 0, RelativePath: filepath.Join(filepath.Dir(absolute), "staging", "..", "shared")},
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "instances.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(dataDir); !errors.Is(err, ErrInstancePathRegistered) {
		t.Fatalf("OpenRegistry error = %v, want ErrInstancePathRegistered", err)
	}
}
