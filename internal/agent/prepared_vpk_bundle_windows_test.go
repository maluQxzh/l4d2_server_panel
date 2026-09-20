//go:build windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestPreparedVPKBundleRejectsWindowsJunction(t *testing.T) {
	e, data, instance := preparedExecutor(t)
	job, bundle := prepareTestVPKBundle(t, e, true)
	cache := filepath.Join(data, "prepared-vpk-bundles", preparedBundleFile(job.ID, 0))
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	createManagedWindowsTestJunction(t, cache, outside)
	if _, err := e.Execute(context.Background(), bundleInstallJob(job, bundle), nil); err == nil {
		t.Fatal("junction accepted as prepared VPK")
	}
	assertFileContent(t, marker, "outside")
	target, _ := domain.PreparedVPKBundleTarget(job.ID, 0, bundle.Files[0].Name)
	if _, err := os.Stat(filepath.Join(instance, filepath.FromSlash(target))); !os.IsNotExist(err) {
		t.Fatal("unsafe VPK was installed")
	}
}
