package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestScanInstalledPackagesValidatesOnlyDirectOrdinaryVPKs(t *testing.T) {
	executor, _, instancePath := uploadedPackageExecutor(t)
	addons := filepath.Join(instancePath, "left4dead2", "addons")
	if err := os.MkdirAll(filepath.Join(addons, "workshop"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"campaign.vpk":            validTestVPK([]byte("campaign"), 2),
		"broken.vpk":              []byte("not a VPK"),
		"l4d2panel-map-one-1.vpk": validTestVPK([]byte("managed map"), 1),
		"readme.txt":              []byte("ignored"),
	} {
		if err := os.WriteFile(filepath.Join(addons, name), body, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(addons, "workshop", "nested.vpk"), validTestVPK([]byte("nested"), 1), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := executor.Execute(context.Background(), domain.Job{ID: "scan", InstanceID: "instance", Type: "scan_packages"}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	packages, ok := result["packages"].([]any)
	if !ok || len(packages) != 1 {
		t.Fatalf("packages = %#v", result["packages"])
	}
	item, _ := packages[0].(map[string]any)
	if item["name"] != "campaign.vpk" || item["path"] != "left4dead2/addons/campaign.vpk" || item["vpk_version"] != 2 {
		t.Fatalf("unexpected scanned package: %#v", item)
	}
	if sha, _ := item["sha256"].(string); len(sha) != 64 {
		t.Fatalf("invalid scanned SHA-256: %q", sha)
	}
	rejected, ok := result["rejected"].([]any)
	if !ok || len(rejected) != 1 {
		t.Fatalf("rejected = %#v", result["rejected"])
	}
	if result["discovered_count"] != 2 || result["valid_count"] != 1 || result["rejected_count"] != 1 {
		t.Fatalf("unexpected scan counts: %#v", result)
	}
	if agentJobRequiresInstanceLock("scan_packages", nil) {
		t.Fatal("read-only installed package scan acquired the Agent instance write lock")
	}
}

func TestScanInstalledPackagesTreatsMissingDirectoryAsEmpty(t *testing.T) {
	executor, _, _ := uploadedPackageExecutor(t)
	result, err := executor.scanInstalledPackages(context.Background(), domain.Job{InstanceID: "instance"}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["discovered_count"] != 0 || result["valid_count"] != 0 {
		t.Fatalf("unexpected empty scan result: %#v", result)
	}
}
