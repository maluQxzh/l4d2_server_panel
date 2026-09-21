package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func readyPreparedOfficialExtension(t *testing.T, s *Store) domain.Job {
	t.Helper()
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "prepare_package", map[string]any{"kind": "metamod", "official": true}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	meta := domain.PreparedPackage{ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Name: "mmsource-1.12-windows.zip", Kind: "metamod", Official: true, Version: "1.12", SHA256: strings.Repeat("a", 64), Size: 100, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	raw, _ := json.Marshal(meta)
	_ = json.Unmarshal(raw, &job.Result)
	job, err = s.UpdateJob(context.Background(), job.ID, domain.JobSucceeded, 100, "ready", job.Result)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPreparedOfficialExtensionQueueListAndProjection(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	p := installQueuePrincipal(t, s)
	prepared := readyPreparedOfficialExtension(t, s)
	server := recoveryServer(t, s)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("instanceID", "instance")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	res := httptest.NewRecorder()
	server.handlePreparedPackages(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"ready":true`) || !strings.Contains(res.Body.String(), `"official":true`) || !strings.Contains(res.Body.String(), `"version":"1.12"`) {
		t.Fatal("official list projection", res.Body.String())
	}
	queue := submitInstallQueue(t, s, p, prepared.ID)
	if changed, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true, true, true); changed || err != nil {
		t.Fatal("old Agent advanced", changed, err)
	}
	if queueChild(t, s, queue.ID, 0).ID != "" {
		t.Fatal("old Agent holds writer")
	}
	if _, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	child := queueChild(t, s, queue.ID, 0)
	if !boolFromAny(child.Payload["official"]) || child.Payload["version"] != "1.12" {
		t.Fatal("official input lost", child.Payload)
	}
	child.Status = domain.JobSucceeded
	child.Result = map[string]any{"kind": "metamod", "official": true, "version": "1.12", "sha256": strings.Repeat("a", 64), "size": 100, "verified": true, "verification_command": "meta version", "managed_files": []any{map[string]any{"path": "left4dead2/addons/metamod/bin/server.dll", "sha256": strings.Repeat("b", 64), "size": 10}}}
	for _, field := range []string{"official", "version"} {
		bad := child
		bad.Result = cloneMap(child.Result)
		delete(bad.Result, field)
		if err := s.ApplyJobResult(ctx, bad); err == nil {
			t.Fatal("unbound result accepted", field)
		}
	}
	for range 2 {
		if err := s.ApplyJobResult(ctx, child); err != nil {
			t.Fatal(err)
		}
	}
	var version string
	if err := s.db.QueryRow(`SELECT version FROM addon_packages WHERE instance_id='instance' AND kind='metamod'`).Scan(&version); err != nil || version != "1.12" {
		t.Fatal("version not projected", version, err)
	}
}

func TestPreparedOfficialExtensionCannotPromoteThirdParty(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	prepared := readyPreparedExtension(t, s, "metamod")
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": prepared.ID, "official": true, "version": "1.12"}, "", ""); err == nil {
		t.Fatal("caller official flag bypassed consent")
	}
	prepared.Result["official"], prepared.Result["version"] = true, "1.12"
	if _, err := preparedMetadata(prepared); err == nil {
		t.Fatal("Agent result promoted third-party preparation")
	}
	official := readyPreparedOfficialExtension(t, s)
	for _, version := range []string{"", "bad\nversion", strings.Repeat("v", 129)} {
		official.Result["version"] = version
		if _, err := preparedMetadata(official); err == nil {
			t.Fatal("invalid official version accepted")
		}
	}
}

func TestPreparedOfficialExtensionHTTPRejectsCallerSources(t *testing.T) {
	s := packageRenameStore(t)
	server := recoveryServer(t, s)
	for _, body := range []string{`{"kind":"native","official":true}`, `{"kind":"metamod","official":true,"source":"https://example.com/package.zip"}`, `{"kind":"metamod","official":true,"content_base64":"YWJj"}`, `{"kind":"metamod","official":true,"name":"fake.zip"}`, `{"kind":"metamod","official":true,"source_transport":"qq_flash"}`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		res := httptest.NewRecorder()
		server.handlePreparePackage(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("caller-supplied official source accepted: %d %s", res.Code, res.Body)
		}
	}
}
