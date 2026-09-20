//go:build windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSmartPreviewRejectsWindowsJunction(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	cached := filepath.Join(e.config.DataDir, smartPreviewDirectory, e.smartPreviews[id].sources["sample.smx"])
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	createManagedWindowsTestJunction(t, cached, outside)
	e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
	if _, err := e.Execute(context.Background(), job, nil); err == nil {
		t.Fatal("junction accepted as a cached plugin file")
	}
	assertFileContent(t, marker, "outside")
	if _, err := os.Stat(filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx")); !os.IsNotExist(err) {
		t.Fatalf("junction changed the instance: %v", err)
	}
}

func TestSmartPreviewRejectsWindowsMetadataJunction(t *testing.T) {
	e, instance, job, id := smartPreviewFixture(t)
	meta := filepath.Join(e.config.DataDir, smartPreviewDirectory, id+".json")
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	createManagedWindowsTestJunction(t, meta, outside)
	e = NewExecutor(e.config, e.paths, e.registry, e.processes, e.logger)
	payload, _ := decodeOperation(job)
	if _, hit, _ := e.installSmartPreview(context.Background(), job, payload, instance, func(int, string) {}); hit {
		t.Fatal("metadata junction was restored")
	}
	assertFileContent(t, marker, "outside")
}
