//go:build windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedWorkshopRejectsWindowsJunction(t *testing.T) {
	e, data, instance := preparedExecutor(t)
	job, bundle := prepareTestWorkshop(t, e, true)
	cache := filepath.Join(data, "prepared-workshop", preparedBundleFile(job.ID, 0))
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	createManagedWindowsTestJunction(t, cache, outside)
	if _, err := e.Execute(context.Background(), workshopInstallJob(job, bundle), nil); err == nil {
		t.Fatal("junction accepted as prepared VPK")
	}
	assertFileContent(t, marker, "outside")
	target, _ := bundle.Files[0].Target()
	if _, err := os.Stat(filepath.Join(instance, filepath.FromSlash(target))); !os.IsNotExist(err) {
		t.Fatal("unsafe VPK was installed")
	}
}
