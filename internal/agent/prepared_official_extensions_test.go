package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func officialPreparationFixture(t *testing.T, e *Executor) (domain.Job, OfficialPackage, []byte) {
	t.Helper()
	body := smartTestZIP(t, map[string]string{"addons/metamod/bin/server.dll": "native"})
	hash := sha256.Sum256(body)
	name := "mmsource-1.12.0-git1234-windows.zip"
	if runtime.GOOS == "linux" {
		name = "mmsource-1.12.0-git1234-linux.tar.gz"
	}
	resolved := OfficialPackage{Kind: "metamod", OS: runtime.GOOS, Version: "1.12.0-git1234", URL: "https://github.com/alliedmodders/metamod-source/releases/download/1.12.0/" + name, ExpectedSHA256: hex.EncodeToString(hash[:]), ExpectedSize: int64(len(body))}
	e.officialPackageResolver = func(context.Context, string) (OfficialPackage, error) { return resolved, nil }
	e.packageDownloader = func(_ context.Context, _, dest string, official bool) (string, int64, error) {
		if !official {
			t.Error("official policy lost")
		}
		return resolved.ExpectedSHA256, resolved.ExpectedSize, os.WriteFile(dest, body, 0600)
	}
	return domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "prepare_package", Payload: map[string]any{"kind": "metamod", "official": true}}, resolved, body
}

func TestPreparedOfficialExtensionRestartPinsVersionAndRollsBack(t *testing.T) {
	e, data, instance := preparedExecutor(t)
	job, resolved, _ := officialPreparationFixture(t, e)
	unlock, err := e.locks.acquire(context.Background(), "instance")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := e.Execute(ctx, job, nil)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if result["official"] != true || result["version"] != resolved.Version {
		t.Fatal("origin/version lost", result)
	}
	e.preparedActive = nil
	if err := e.cleanupPreparedExtensions(time.Now()); err != nil {
		t.Fatal(err)
	}
	e.officialPackageResolver = func(context.Context, string) (OfficialPackage, error) {
		t.Error("resolved latest again")
		return OfficialPackage{}, errors.New("must use snapshot")
	}
	e.packageDownloader = func(context.Context, string, string, bool) (string, int64, error) {
		t.Error("downloaded again")
		return "", 0, errors.New("must use snapshot")
	}
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
	install := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "install_mod", Payload: map[string]any{"prepared_package_id": job.ID, "kind": "metamod", "official": true, "version": result["version"], "name": result["name"], "expected_sha256": result["sha256"], "expected_size": result["size"], "rcon_password": "test-secret"}}
	installed := false
	_, err = e.Execute(context.Background(), install, func(_ int, msg string) {
		if msg == "starting server for extension verification" {
			installed = true
		}
	})
	if !installed || err == nil || !strings.Contains(err.Error(), "was rolled back") {
		t.Fatalf("official install without third-party consent: %v %v", installed, err)
	}
	raw, err := os.ReadFile(original)
	if err != nil || string(raw) != "old" {
		t.Fatal("rollback failed", err)
	}
	if _, err := os.Stat(filepath.Join(data, "prepared-official-extensions", job.ID+".bin")); !os.IsNotExist(err) {
		t.Fatal("claimed snapshot retained", err)
	}
}

func TestPreparedOfficialExtensionRejectsInvalidOriginAndIntegrity(t *testing.T) {
	for _, scenario := range []string{"source", "upload", "name", "transport", "kind", "platform", "version", "asset", "digest", "lying-downloader"} {
		t.Run(scenario, func(t *testing.T) {
			e, data, _ := preparedExecutor(t)
			job, resolved, body := officialPreparationFixture(t, e)
			switch scenario {
			case "source":
				job.Payload["source"] = "https://example.com/untrusted.zip"
			case "upload":
				job.Payload["content_base64"] = "YWJj"
			case "name":
				job.Payload["name"] = "forged.zip"
			case "transport":
				job.Payload["source_transport"] = "qq_flash"
			case "kind":
				job.Payload["kind"] = "native"
			case "platform":
				resolved.OS = "unsupported"
			case "version":
				resolved.Version = "changed"
			case "asset":
				resolved.URL = "https://example.com/official.zip"
			case "digest":
				resolved.ExpectedSHA256 = strings.Repeat("0", 64)
			case "lying-downloader":
				e.packageDownloader = func(_ context.Context, _, dest string, _ bool) (string, int64, error) {
					return resolved.ExpectedSHA256, int64(len(body)), os.WriteFile(dest, []byte("tampered"), 0600)
				}
			}
			e.officialPackageResolver = func(context.Context, string) (OfficialPackage, error) { return resolved, nil }
			if _, err := e.Execute(context.Background(), job, nil); err == nil {
				t.Fatal("invalid official input accepted")
			}
			if _, err := os.Stat(filepath.Join(data, "prepared-official-extensions", job.ID+".json")); !os.IsNotExist(err) {
				t.Fatal("failed preparation retained", err)
			}
		})
	}
}

func TestPreparedOfficialExtensionMirrorRequiresOfficialDigest(t *testing.T) {
	e, _, _ := preparedExecutor(t)
	job, resolved, _ := officialPreparationFixture(t, e)
	resolved.MirrorURLs = []string{"https://gh-proxy.com/" + resolved.URL}
	resolved.PreferMirrors = true
	resolved.ExpectedSHA256 = ""
	resolved.ExpectedSize = 0
	e.officialPackageResolver = func(context.Context, string) (OfficialPackage, error) { return resolved, nil }
	calls := 0
	e.packageDownloader = func(_ context.Context, source, _ string, _ bool) (string, int64, error) {
		calls++
		if strings.Contains(source, "gh-proxy.com") {
			t.Error("unverified mirror attempted")
		}
		return "", 0, errors.New("official unavailable")
	}
	if _, err := e.Execute(context.Background(), job, nil); err == nil || calls != 1 {
		t.Fatalf("mirror fallback: calls=%d err=%v", calls, err)
	}
}

func TestPreparedOfficialExtensionBindsCommitHashAndVersion(t *testing.T) {
	e, data, _ := preparedExecutor(t)
	job, _, _ := officialPreparationFixture(t, e)
	_, err := e.Execute(context.Background(), job, func(percent int, _ string) {
		if percent == 70 {
			if err := os.WriteFile(filepath.Join(data, "prepared-official-extensions", job.ID+".bin"), []byte("changed after verification"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err == nil {
		t.Fatal("verified bytes replaced before metadata commit")
	}
	job, _, _ = officialPreparationFixture(t, e)
	result, err := e.Execute(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	install := domain.Job{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Type: "install_mod", Payload: map[string]any{"prepared_package_id": job.ID, "kind": "metamod", "official": true, "version": "changed", "name": result["name"], "expected_sha256": result["sha256"], "expected_size": result["size"]}}
	if _, err := e.Execute(context.Background(), install, nil); err == nil || !strings.Contains(err.Error(), "metadata mismatch") {
		t.Fatal("forged version consumed", err)
	}
	install.Type = "discard_prepared_package"
	if _, err := e.Execute(context.Background(), install, nil); err != nil {
		t.Fatal("unclaimed snapshot cannot be discarded", err)
	}
}
