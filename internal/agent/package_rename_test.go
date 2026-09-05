package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func renameTestFile(t *testing.T, root, relative, body string) string {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	sha, _, err := hashFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestRenamePackagePreservesContentStateAndHistory(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			e, data, root := uploadedPackageExecutor(t)
			e.processes = &ProcessManager{items: map[string]*managedProcess{}}
			oldCanonical, newCanonical := "left4dead2/addons/old.vpk", "left4dead2/addons/新的模组.vpk"
			source, target := oldCanonical, newCanonical
			if disabled {
				source = strings.Replace(source, "addons/", "addons/panel-disabled/", 1)
				target = strings.Replace(target, "addons/", "addons/panel-disabled/", 1)
			}
			sha := renameTestFile(t, root, source, "workshop payload deliberately not parsed")
			storage := "package-versions/" + packageVersionKey("instance") + "/" + packageVersionKey(oldCanonical) + "/" + strings.Repeat("a", 64) + ".bin"
			historySHA := renameTestFile(t, data, storage, "previous")
			job := domain.Job{InstanceID: "instance", Payload: map[string]any{"package_id": "package", "name": "新名称", "kind": "workshop", "path": source, "target_path": target, "expected_sha256": sha, "snapshots": []any{map[string]any{"storage_path": storage, "sha256": historySHA}}}}
			result, err := e.renamePackage(context.Background(), job, func(int, string) {})
			if err != nil || result["path"] != target {
				t.Fatalf("rename: %#v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(root, source)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old file remains: %v", err)
			}
			actual, _, err := hashRegularFileWithinNoFollow(root, target, maxPackageDownload)
			if err != nil || actual != sha {
				t.Fatalf("renamed file: %s %v", actual, err)
			}
			renamed, err := domain.RenamedPackageSnapshot("instance", oldCanonical, newCanonical, storage)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.resolvePackageSnapshot("instance", newCanonical, renamed); err != nil {
				t.Fatal(err)
			}
			actual, _, err = hashRegularFileWithinNoFollow(data, renamed, maxPackageDownload)
			if err != nil || actual != historySHA {
				t.Fatalf("history lost: %s %v", actual, err)
			}
		})
	}
}

func TestRenamePackageRejectsUnsafeMutation(t *testing.T) {
	for _, scenario := range []string{"running", "collision", "disabled_collision", "changed_hash", "source_link", "parent_link", "snapshot_link", "snapshot_foreign"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, root := uploadedPackageExecutor(t)
			e.processes = &ProcessManager{items: map[string]*managedProcess{}}
			source, target := "left4dead2/addons/old.vpk", "left4dead2/addons/new.vpk"
			sha := renameTestFile(t, root, source, "original")
			job := domain.Job{InstanceID: "instance", Payload: map[string]any{"package_id": "package", "name": "new", "kind": "vpk", "path": source, "target_path": target, "expected_sha256": sha}}
			switch scenario {
			case "running":
				e.processes.items["instance"] = &managedProcess{state: ProcessState{Status: "running"}}
			case "collision":
				renameTestFile(t, root, target, "existing")
			case "disabled_collision":
				renameTestFile(t, root, "left4dead2/addons/panel-disabled/new.vpk", "existing")
			case "changed_hash":
				job.Payload["expected_sha256"] = strings.Repeat("a", 64)
			case "source_link":
				outside := filepath.Join(t.TempDir(), "outside.vpk")
				if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(root, source)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, source)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "parent_link":
				parent := filepath.Join(root, "left4dead2", "addons")
				moved := filepath.Join(root, "moved")
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, parent); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "snapshot_link", "snapshot_foreign":
				storage := "package-versions/" + packageVersionKey("instance") + "/" + packageVersionKey(source) + "/" + strings.Repeat("b", 64) + ".bin"
				if scenario == "snapshot_foreign" {
					storage = strings.Replace(storage, packageVersionKey("instance"), packageVersionKey("other"), 1)
				}
				hash := renameTestFile(t, data, storage, "snapshot")
				if scenario == "snapshot_link" {
					file := filepath.Join(data, storage)
					outside := filepath.Join(t.TempDir(), "snapshot")
					if err := os.Rename(file, outside); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, file); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				job.Payload["snapshots"] = []any{map[string]any{"storage_path": storage, "sha256": hash}}
			}
			if _, err := e.renamePackage(context.Background(), job, func(int, string) {}); err == nil {
				t.Fatal("unsafe rename succeeded")
			}
			if data, err := os.ReadFile(filepath.Join(root, source)); err != nil || string(data) != "original" {
				t.Fatalf("source changed: %q %v", data, err)
			}
		})
	}
}

func TestRenamePackageDisplayOnlyDoesNotTouchFiles(t *testing.T) {
	e, _, _ := uploadedPackageExecutor(t)
	job := domain.Job{InstanceID: "instance", Payload: map[string]any{"package_id": "smart", "kind": "smart", "name": "中文显示名称", "path": "", "target_path": "", "expected_sha256": ""}}
	if result, err := e.renamePackage(context.Background(), job, func(int, string) {}); err != nil || result["name"] != "中文显示名称" {
		t.Fatalf("display rename: %#v %v", result, err)
	}
}
