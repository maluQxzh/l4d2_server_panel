package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

func readyPreparedJob(t *testing.T, s *Store) domain.Job {
	t.Helper()
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "prepare_package", map[string]any{"kind": "vpk", "name": "ready.vpk"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	meta := domain.PreparedPackage{ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Name: "ready.vpk", Kind: "vpk", SHA256: strings.Repeat("a", 64), Size: 100, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	raw, _ := json.Marshal(meta)
	json.Unmarshal(raw, &job.Result)
	job, err = s.UpdateJob(context.Background(), job.ID, domain.JobSucceeded, 100, "ready", job.Result)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPreparedPackageQueueAndConsumption(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	write, _, err := s.QueueInstanceJob(ctx, "instance", "stop", nil, "", "stop")
	if err != nil {
		t.Fatal(err)
	}
	p1 := readyPreparedJob(t, s)
	p2 := readyPreparedJob(t, s)
	payload := map[string]any{"prepared_package_id": p1.ID, "kind": "native", "name": "forged.dll", "expected_sha256": "forged"}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", payload, "", "install"); !errors.Is(err, ErrConflict) {
		t.Fatalf("write barrier bypassed: %v", err)
	}
	if _, err := s.UpdateJob(ctx, write.ID, domain.JobSucceeded, 100, "stopped", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET result='{}' WHERE id=?1`, write.ID); err != nil {
		t.Fatal(err)
	}
	install, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", payload, "", "install")
	if err != nil {
		t.Fatal(err)
	}
	if install.Payload["kind"] != "vpk" || install.Payload["name"] != "ready.vpk" || install.Payload["expected_sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("caller metadata trusted: %#v", install)
	}
	replay, reused, err := s.QueueInstanceJob(ctx, "instance", "install_mod", payload, "", "install")
	if err != nil || !reused || replay.ID != install.ID {
		t.Fatal("idempotent replay lost")
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "discard_prepared_package", map[string]any{"prepared_package_id": p1.ID}, "", "discard-claimed"); !errors.Is(err, ErrConflict) {
		t.Fatal("double consumption accepted")
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "discard_prepared_package", map[string]any{"prepared_package_id": p2.ID}, "", "discard"); err != nil {
		t.Fatalf("discard incorrectly takes instance lock: %v", err)
	}
	install.Status = domain.JobSucceeded
	install.Result = map[string]any{"kind": "vpk", "path": "left4dead2/addons/forged.vpk", "sha256": strings.Repeat("a", 64), "size": 100}
	if err := s.ApplyJobResult(ctx, install); err == nil {
		t.Fatal("forged prepared projection accepted")
	}
}

func TestPreparedPackagesRejectCrossInstanceAndExpiry(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	job := readyPreparedJob(t, s)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO instances(id,node_id,name,status,path,port,config) VALUES('other','node','Other','stopped','/other',27025,'{}')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "other", "install_mod", map[string]any{"prepared_package_id": job.ID}, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-instance install: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET result=json_set(result,'$.expires_at',?2) WHERE id=?1`, job.ID, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": job.ID}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired install: %v", err)
	}
}

func TestPreparedPackagePermissionAndSecretStorage(t *testing.T) {
	s := packageRenameStore(t)
	server := recoveryServer(t, s)
	for _, handler := range []http.HandlerFunc{server.handlePreparePackage, server.handleConsumePreparedPackage, server.handlePreparedPackages} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", "instance")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		req = req.WithContext(context.WithValue(req.Context(), principalContextKey, Principal{User: domain.User{ID: "viewer", Role: domain.RoleViewer}}))
		res := httptest.NewRecorder()
		server.requireInstancePermission(PermissionPackagesInstall)(handler).ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("unprivileged prepared endpoint: %d", res.Code)
		}
	}
	payload := map[string]any{"source": "https://example.com/file.vpk?secret=hidden"}
	if err := sealJobPayloadSecrets(server.config.MasterKey, "prepare_package", payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stringFromAny(payload["source"]), "hidden") || payload["source_ciphertext"] == nil {
		t.Fatal("source secret persisted in clear")
	}
	if err := unsealJobPayloadSecrets(server.config.MasterKey, "prepare_package", payload); err != nil || !strings.Contains(stringFromAny(payload["source"]), "hidden") {
		t.Fatal("preparation source could not be dispatched")
	}
}

func TestPreparedPackageSlotsAndCapabilityGate(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	var first domain.Job
	for i := 0; i < domain.PreparedPackageSlots; i++ {
		job, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", map[string]any{"kind": "vpk", "name": "file.vpk"}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = job
		}
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", nil, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("capacity exceeded: %v", err)
	}
	server := recoveryServer(t, s)
	// A legacy connection has no websocket: sending an unsupported job would
	// panic. All preparations must remain queued until a capable Agent returns.
	server.agents.conns["node"] = &agentConnection{capabilities: map[string]struct{}{}}
	server.agents.Dispatch(ctx, "node")
	delete(server.agents.conns, "node")
	queued, err := s.GetJob(ctx, first.ID)
	if err != nil || queued.Status != domain.JobQueued {
		t.Fatalf("legacy Agent received preparation: %v", err)
	}
	if _, err := s.MarkDispatched(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CancelJob(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", nil, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel pending released capacity: %v", err)
	}
	if _, err := s.UpdateJob(ctx, first.ID, domain.JobCanceled, 100, "canceled", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", nil, "", ""); err != nil {
		t.Fatalf("confirmed cancellation did not release capacity: %v", err)
	}
}

func TestPreparedPackageHTTPInstallListAndReplay(t *testing.T) {
	s := packageRenameStore(t)
	server := recoveryServer(t, s)
	job := readyPreparedJob(t, s)
	request := func(method string, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "prepared-http-install")
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", "instance")
		route.URLParams.Add("jobID", job.ID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		req = req.WithContext(context.WithValue(req.Context(), principalContextKey, Principal{User: domain.User{Role: domain.RoleOwner}}))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	list := request(http.MethodGet, server.handlePreparedPackages, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"ready":true`) {
		t.Fatalf("ready list: %s", list.Body)
	}
	first := request(http.MethodPost, server.handleConsumePreparedPackage, `{}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", first.Code, first.Body)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET result=json_set(result,'$.expires_at',?2) WHERE id=?1`, job.ID, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	replay := request(http.MethodPost, server.handleConsumePreparedPackage, `{}`)
	if replay.Code != http.StatusAccepted || !strings.Contains(replay.Body.String(), `"idempotent_replay":true`) {
		t.Fatalf("replay after expiry: %d %s", replay.Code, replay.Body)
	}
	list = request(http.MethodGet, server.handlePreparedPackages, "")
	if !strings.Contains(list.Body.String(), `"ready":false`) || !strings.Contains(list.Body.String(), `"busy":true`) {
		t.Fatalf("claim list: %s", list.Body)
	}
	unsupported := request(http.MethodPost, server.handlePreparePackage, `{"kind":"vpk","source":"https://example.com/file.vpk"}`)
	if unsupported.Code != http.StatusConflict || !strings.Contains(unsupported.Body.String(), "agent_upgrade_required") {
		t.Fatalf("unsupported preparation: %d %s", unsupported.Code, unsupported.Body)
	}
}

func TestPreparedPackageHTTPDispatchPreservesSource(t *testing.T) {
	s := packageRenameStore(t)
	server := recoveryServer(t, s)
	node, err := s.GetNode(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { server.agents.Serve(node, "127.0.0.1", w, r) }))
	defer endpoint.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	hello, err := protocol.NewEnvelope(protocol.TypeHello, "node", "", protocol.Hello{Capabilities: []string{protocol.CapabilityPreparedPackagesV1, "absolute_game_paths"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !server.agents.Supports("node", protocol.CapabilityPreparedPackagesV1) {
		if time.Now().After(deadline) {
			t.Fatal("Agent capability not registered")
		}
		time.Sleep(time.Millisecond)
	}
	source := "https://example.com/MixedCase.vpk?Token=SecretCASE"
	body, _ := json.Marshal(map[string]any{"kind": "vpk", "source": source})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	route := chi.NewRouteContext()
	route.URLParams.Add("instanceID", "instance")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	req = req.WithContext(context.WithValue(req.Context(), principalContextKey, Principal{User: domain.User{Role: domain.RoleOwner}}))
	res := httptest.NewRecorder()
	server.handlePreparePackage(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("prepare: %d %s", res.Code, res.Body)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var envelope protocol.Envelope
	if err := connection.ReadJSON(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Type != protocol.TypeJob {
		t.Fatalf("unexpected envelope: %s", envelope.Type)
	}
	var command protocol.JobCommand
	if err := json.Unmarshal(envelope.Payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.Job.Type != "prepare_package" || command.Job.Payload["source"] != source || command.Job.Payload["name"] != "MixedCase.vpk" {
		t.Fatalf("dispatch metadata changed: %#v", command.Job.Payload)
	}
	stored, err := s.GetJob(context.Background(), command.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Payload["source"] == source || stored.Payload["source_ciphertext"] == nil {
		t.Fatal("source not encrypted at rest")
	}
}
