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
	cached := filepath.Join(e.config.DataDir, "smart-previews", e.smartPreviews[id].sources["sample.smx"])
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	createManagedWindowsTestJunction(t, cached, outside)
	if _, err := e.Execute(context.Background(), job, nil); err == nil {
		t.Fatal("junction accepted as a cached plugin file")
	}
	assertFileContent(t, marker, "outside")
	if _, err := os.Stat(filepath.Join(instance, "left4dead2/addons/sourcemod/plugins/sample.smx")); !os.IsNotExist(err) {
		t.Fatalf("junction changed the instance: %v", err)
	}
}
