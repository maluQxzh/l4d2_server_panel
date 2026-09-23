package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func prepareExtensionTest(t *testing.T, e *Executor, kind, transport string) (domain.Job, domain.Job) {
	t.Helper()
	body := smartTestZIP(t, map[string]string{"addons/metamod/bin/server.dll": "native"})
	p := map[string]any{"kind": kind, "name": "extension.zip", "content_base64": base64.StdEncoding.EncodeToString(body)}
	if transport != "" {
		delete(p, "content_base64")
		p["source"] = "https://example.com/extension.zip"
		if transport == "qq_flash" {
			delete(p, "name")
			p["source"], p["source_transport"] = "https://qfile.qq.com/q/SecretCASE", transport
			downloaded := 0
			e.preparedQQClient = preparedQQTestClient(t, e, []string{"extension.zip"}, int64(len(body)), body, &downloaded)
		} else {
			e.packageDownloader = func(_ context.Context, _, dest string, official bool) (string, int64, error) {
				if official {
					t.Error("caller source treated as official")
				}
				return "", 0, os.WriteFile(dest, body, 0600)
			}
		}
	}
	job := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: p}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := e.Execute(ctx, job, nil)
	if err != nil {
		t.Fatal(err)
	}
	return job, domain.Job{ID: uuid.NewString(), NodeID: job.NodeID, InstanceID: job.InstanceID, Type: "install_mod", Payload: map[string]any{"prepared_package_id": job.ID, "kind": kind, "name": "extension.zip", "expected_sha256": result["sha256"], "expected_size": result["size"], "acknowledge_native_risk": true}}
}

func TestPreparedExtensionSourcesAndRestart(t *testing.T) {
	for _, transport := range []string{"", "https", "qq_flash"} {
		t.Run(transport, func(t *testing.T) {
			e, data, instance := preparedExecutor(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			unlock, err := e.locks.acquire(ctx, "instance")
			if err != nil {
				t.Fatal(err)
			}
			job, install := prepareExtensionTest(t, e, "sourcemod", transport)
			unlock()
			if entries, err := os.ReadDir(instance); err != nil || len(entries) != 0 {
				t.Fatal("preparation changed instance", err)
			}
			restarted := &Executor{config: e.config, paths: e.paths, registry: e.registry, logger: e.logger, locks: newKeyedLocker()}
			if err := restarted.cleanupPreparedExtensions(time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := restarted.cleanupPreparedPackages(time.Now()); err != nil {
				t.Fatal(err)
			}
			// No cached dependency decision: SourceMod preparation is allowed before
			// Metamod, but consumption rechecks real loader files after restart.
			if _, err := restarted.Execute(ctx, install, nil); err == nil || !strings.Contains(err.Error(), "requires an installed Metamod") {
				t.Fatalf("dependency: %v", err)
			}
			if _, err := os.Stat(filepath.Join(data, "prepared-extensions", job.ID+".bin")); !os.IsNotExist(err) {
				t.Fatal("claimed archive retained", err)
			}
			if _, err := restarted.Execute(ctx, install, nil); err == nil {
				t.Fatal("claim reused")
			}
		})
	}
}

func TestPreparedExtensionRejectsRiskTamperBindingAndExpiry(t *testing.T) {
	for _, scenario := range []string{"risk", "rcon", "bytes", "staged", "binding", "expiry", "name"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, _ := preparedExecutor(t)
			job, install := prepareExtensionTest(t, e, "metamod", "")
			root := filepath.Join(data, "prepared-extensions")
			want := ""
			switch scenario {
			case "risk":
				install.Payload["acknowledge_native_risk"] = false
				want = "风险"
			case "rcon":
				want = "RCON password"
			case "bytes":
				if err := os.WriteFile(filepath.Join(root, job.ID+".bin"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "bytes changed"
			case "staged":
				staged := filepath.Join(root, job.ID+".files", "left4dead2", "addons", "metamod", "bin", "server.dll")
				if err := os.WriteFile(staged, []byte("changed staged"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "staged package file changed"
			case "binding":
				install.NodeID = "other"
				want = "不属于"
			case "name":
				install.Payload["name"] = "other.zip"
				want = "metadata mismatch"
			case "expiry":
				record, err := readPreparedPackage(root, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				record.ExpiresAt = time.Now().Add(-time.Minute)
				if err := savePreparedPackage(root, record); err != nil {
					t.Fatal(err)
				}
				want = "expired"
			}
			if _, err := e.Execute(context.Background(), install, nil); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("wanted %s: %v", want, err)
			}
		})
	}
}

func TestPreparedExtensionInstallFailureRestoresCheckpoint(t *testing.T) {
	e, data, instance := preparedExecutor(t)
	var err error
	e.processes, err = NewProcessManager(data, e.logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(instance, "left4dead2", "addons", "metamod", "bin", "server.dll")
	if err := os.MkdirAll(filepath.Dir(original), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	_, install := prepareExtensionTest(t, e, "metamod", "")
	install.Payload["rcon_password"] = "test-password"
	installed := false
	_, err = e.Execute(context.Background(), install, func(_ int, message string) {
		if message == "starting server for extension verification" {
			raw, err := os.ReadFile(original)
			if err != nil || string(raw) != "native" {
				t.Error("archive did not install", err)
			}
			installed = true
		}
	})
	// There is no game binary in this fixture. Startup must fail after writing
	// the new component, then restore the original file through the real checkpoint.
	if !installed || err == nil || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("install/rollback: installed=%v err=%v", installed, err)
	}
	raw, err := os.ReadFile(original)
	if err != nil || string(raw) != "old" {
		t.Fatal("checkpoint not restored", string(raw), err)
	}
}
