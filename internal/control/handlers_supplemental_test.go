package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestNormalizeQQFlashShareURL(t *testing.T) {
	input := "测试用户通过QQ闪传分享了文件\n链接：[https://qfile.qq.com/q/Example123](https://qfile.qq.com/q/Example123)"
	if got, err := normalizeQQFlashShareURL(input); err != nil || got != "https://qfile.qq.com/q/Example123" {
		t.Fatalf("normalized share URL = %q, %v", got, err)
	}
	for _, value := range []string{"https://evil.example/q/Example123", "https://qfile.qq.com.evil.example/q/Example123",
		"https://qfile.qq.com/q/" + strings.Repeat("a", 65),
		"https://qfile.qq.com/q/first01 https://qfile.qq.com/q/second02"} {
		if _, err := normalizeQQFlashShareURL(value); err == nil {
			t.Fatalf("unsafe QQ share input accepted: %q", value)
		}
	}
}

func TestSafeBroadcastCommand(t *testing.T) {
	command, err := safeBroadcastCommand("Server restarts in five minutes")
	if err != nil {
		t.Fatal(err)
	}
	if command != `say "Server restarts in five minutes"` {
		t.Fatalf("unexpected command %q", command)
	}
	for _, value := range []string{"", "hello; quit", "line\nbreak", `quote"break`} {
		if _, err := safeBroadcastCommand(value); err == nil {
			t.Errorf("expected %q to be rejected", value)
		}
	}
}

func TestValidatePackageUpload(t *testing.T) {
	valid := map[string]any{"name": "campaign.vpk", "content_base64": base64.StdEncoding.EncodeToString([]byte("vpk"))}
	if err := validatePackageUpload(valid); err != nil {
		t.Fatal(err)
	}
	qqPayload := map[string]any{
		"kind": "smart", "source": "QQ 分享：https://qfile.qq.com/q/Example123", "source_transport": "qq_flash",
	}
	if err := validatePackageUpload(qqPayload); err != nil {
		t.Fatalf("valid QQ flash plugin source was rejected: %v", err)
	}
	if qqPayload["source"] != "https://qfile.qq.com/q/Example123" {
		t.Fatalf("QQ flash plugin source was not normalized: %#v", qqPayload["source"])
	}
	previouslyTooLarge := make([]byte, 8<<20+1)
	if err := validatePackageUpload(map[string]any{
		"name": "large-package.zip", "content_base64": base64.StdEncoding.EncodeToString(previouslyTooLarge),
	}); err != nil {
		t.Fatalf("package above the previous 8 MiB limit was rejected: %v", err)
	}
	for _, payload := range []map[string]any{
		{"name": "../plugin.smx", "content_base64": "YQ=="},
		{"name": "plugin.smx", "source": "https://example.test/p.smx", "content_base64": "YQ=="},
		{"name": "plugin.smx", "content_base64": "not-base64"},
		{"kind": "workshop", "source": "https://qfile.qq.com/q/Example123", "source_transport": "qq_flash"},
		{"kind": "smart", "source_transport": "qq_flash"},
		{"kind": "smart", "source": "https://qfile.qq.com/q/Example123", "source_transport": "ftp"},
		{"kind": "smart", "source": "https://qfile.qq.com/q/Example123", "source_transport": "qq_flash", "content_base64": "YQ=="},
	} {
		if err := validatePackageUpload(payload); err == nil {
			t.Fatalf("unsafe upload was accepted: %#v", payload)
		}
	}
}

func TestValidateSmartPluginInstallAllowsQQFlashArchive(t *testing.T) {
	payload := map[string]any{
		"kind": "smart", "source": "https://qfile.qq.com/q/Example123", "source_transport": "qq_flash",
		"inspection_fingerprint": strings.Repeat("a", 64), "smart_packages": []any{map[string]any{"folder_id": "plugin"}},
	}
	if err := validateSmartPluginInstallPayload(domain.Instance{Status: domain.InstanceStopped}, payload); err != nil {
		t.Fatalf("QQ flash smart plugin install was rejected: %v", err)
	}
	if payload["path_encoding"] != domain.MapPathEncodingAuto {
		t.Fatalf("default smart plugin path encoding = %#v", payload["path_encoding"])
	}
	payload["source_transport"] = ""
	if err := validateSmartPluginInstallPayload(domain.Instance{Status: domain.InstanceStopped}, payload); err == nil {
		t.Fatal("ordinary remote smart plugin source was accepted")
	}
	invalid := map[string]any{
		"kind": "smart", "name": "plugins.zip", "content_base64": "YQ==", "path_encoding": "shift_jis",
		"inspection_fingerprint": strings.Repeat("a", 64), "smart_packages": []any{map[string]any{"folder_id": "plugin"}},
	}
	if err := validateSmartPluginInstallPayload(domain.Instance{Status: domain.InstanceStopped}, invalid); err == nil {
		t.Fatal("unsupported smart plugin path encoding was accepted")
	}
}

func TestNormalizePluginArchivePathEncoding(t *testing.T) {
	t.Parallel()
	for input, expected := range map[string]string{"": "auto", " AUTO ": "auto", "UTF8": "utf8", "gbk": "gbk", "Big5": "big5"} {
		actual, err := normalizePluginArchivePathEncoding(input)
		if err != nil || actual != expected {
			t.Errorf("normalizePluginArchivePathEncoding(%q) = %q, %v", input, actual, err)
		}
	}
	if _, err := normalizePluginArchivePathEncoding("shift_jis"); err == nil {
		t.Fatal("unsupported plugin archive path encoding was accepted")
	}
}

func TestParseConnectedDuration(t *testing.T) {
	if value, ok := parseConnectedDuration("01:23"); !ok || value != time.Minute+23*time.Second {
		t.Fatalf("unexpected duration %v, %v", value, ok)
	}
	if value, ok := parseConnectedDuration("02:01:23"); !ok || value != 2*time.Hour+time.Minute+23*time.Second {
		t.Fatalf("unexpected duration %v, %v", value, ok)
	}
	if _, ok := parseConnectedDuration("bad; quit"); ok {
		t.Fatal("invalid connected duration was accepted")
	}
}

func TestRCONPlayerRefreshRequiresRunningServerAndCredential(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.InstanceStatus{domain.InstanceRunning, domain.InstanceDegraded} {
		if !rconPlayerRefreshAllowed(domain.Instance{Status: status, RCONConfigured: true}) {
			t.Fatalf("RCON refresh rejected status %s", status)
		}
	}
	for _, instance := range []domain.Instance{
		{Status: domain.InstanceStopped, RCONConfigured: true},
		{Status: domain.InstanceRunning, RCONConfigured: false},
		{Status: domain.InstanceExternal, RCONConfigured: true},
	} {
		if rconPlayerRefreshAllowed(instance) {
			t.Fatalf("RCON refresh accepted instance %#v", instance)
		}
	}
}

func TestRCONPlayerRefreshIsQueuedAsBackgroundProbe(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "rcon-refresh.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "players", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	request := httptest.NewRequest("GET", "/api/v1/instances/"+instance.ID+"/players", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{User: owner}))

	server.queueRefresh(request, instance.ID, "rcon.players", map[string]any{})

	var requestedBy string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(requested_by,'') FROM jobs WHERE instance_id=?1 AND type='rcon.players'`, instance.ID).Scan(&requestedBy); err != nil {
		t.Fatal(err)
	}
	if requestedBy != "" {
		t.Fatalf("RCON player refresh requester = %q, want system background probe", requestedBy)
	}
}

func TestInstancePlayersReportLoadingUntilRCONProbeCompletes(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "player-refresh.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "players", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	encryptedRCON, err := encryptSecret(masterKey, "rcon-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveInstanceSecrets(ctx, instance.ID, encryptedRCON, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='running' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{MasterKey: masterKey}, store, nil)
	defer server.Close()
	request := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/instances/"+instance.ID+"/players", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", instance.ID)
		requestContext := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		requestContext = context.WithValue(requestContext, principalContextKey, Principal{User: owner})
		req = req.WithContext(requestContext)
		recorder := httptest.NewRecorder()
		server.handleInstancePlayers(recorder, req)
		return recorder
	}

	loading := request()
	if loading.Code != http.StatusAccepted {
		t.Fatalf("initial player list status=%d body=%s", loading.Code, loading.Body.String())
	}
	var pending struct {
		Status       string `json:"status"`
		RefreshJobID string `json:"refresh_job_id"`
	}
	if err := json.Unmarshal(loading.Body.Bytes(), &pending); err != nil || pending.Status != "loading" || pending.RefreshJobID == "" {
		t.Fatalf("initial player list response=%#v err=%v", pending, err)
	}
	result := `{"players":[{"user_id":"7","name":"Coach","steam_id":"STEAM_1:1:12345","score":42,"ping":35,"bot":false,"connected":"01:23"}]}`
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded',progress=100,result=?2,finished_at=?3 WHERE id=?1`, pending.RefreshJobID, result, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	ready := request()
	if ready.Code != http.StatusOK {
		t.Fatalf("completed player list status=%d body=%s", ready.Code, ready.Body.String())
	}
	var completed struct {
		Status string           `json:"status"`
		Items  []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(ready.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" || len(completed.Items) != 1 || completed.Items[0]["name"] != "Coach" || completed.Items[0]["score"] != float64(42) || completed.Items[0]["connected_at"] == nil {
		t.Fatalf("completed player list response=%#v", completed)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped',players=5,pid=123 WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	stopped := request()
	if stopped.Code != http.StatusOK || !strings.Contains(stopped.Body.String(), `"items":[]`) {
		t.Fatalf("stopped players: %s", stopped.Body.String())
	}
	projected, err := store.GetInstance(ctx, instance.ID)
	if err != nil || projected.Players != 0 || projected.PID != 0 {
		t.Fatalf("legacy stopped snapshot: %+v %v", projected, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET config=json_set(config,'$.public_status',json('true')) WHERE id=?1;
		UPDATE nodes SET public_address='2001:db8::1' WHERE id='node'`, instance.ID); err != nil {
		t.Fatal(err)
	}
	public, err := store.PublicServers(ctx)
	if err != nil || len(public) != 1 || public[0].Players != 0 || public[0].Address != "[2001:db8::1]:27015" {
		t.Fatalf("public stopped snapshot: %+v %v", public, err)
	}
	if err := store.ApplyJobResult(ctx, domain.Job{InstanceID: instance.ID, Type: "stop", Status: domain.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	probe := domain.Job{InstanceID: instance.ID, Type: "a2s.info", Status: domain.JobSucceeded, CreatedAt: time.Now().Add(-time.Hour),
		Result: map[string]any{"info": map[string]any{"map": "late", "players": 3, "max_players": 8}}}
	if err := store.ApplyJobResult(ctx, probe); err != nil {
		t.Fatal(err)
	}
	var storedPlayers int
	if err := store.db.QueryRowContext(ctx, `SELECT players FROM instances WHERE id=?1`, instance.ID).Scan(&storedPlayers); err != nil || storedPlayers != 0 {
		t.Fatalf("late probe restored stopped players: %d %v", storedPlayers, err)
	}
	started := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='running',started_at=?2 WHERE id=?1`, instance.ID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyJobResult(ctx, probe); err != nil {
		t.Fatal(err)
	}
	projected, err = store.GetInstance(ctx, instance.ID)
	if err != nil || projected.Players != 0 {
		t.Fatalf("previous generation probe accepted: %+v %v", projected, err)
	}
	restarted := request()
	var newPending struct {
		Status       string `json:"status"`
		RefreshJobID string `json:"refresh_job_id"`
	}
	if err := json.Unmarshal(restarted.Body.Bytes(), &newPending); err != nil || newPending.Status != "loading" || newPending.RefreshJobID == pending.RefreshJobID {
		t.Fatalf("restart reused old player cache: %s %v", restarted.Body.String(), err)
	}
	probe.CreatedAt = time.Now().UTC()
	if err := store.ApplyJobResult(ctx, probe); err != nil {
		t.Fatal(err)
	}
	projected, err = store.GetInstance(ctx, instance.ID)
	if err != nil || projected.Players != 3 {
		t.Fatalf("fresh probe was not applied: %+v %v", projected, err)
	}
	if err := store.Heartbeat(ctx, "node", "test", map[string]string{instance.ID: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT players FROM instances WHERE id=?1`, instance.ID).Scan(&storedPlayers); err != nil || storedPlayers != 0 {
		t.Fatalf("heartbeat retained players: %d %v", storedPlayers, err)
	}
}

func TestInstanceFilesReportLoadingUntilAgentListingCompletes(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "file-list-refresh.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "files", Port: 27015})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{MasterKey: []byte("0123456789abcdef0123456789abcdef")}, store, nil)
	defer server.Close()
	request := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/instances/"+instance.ID+"/files?path=", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", instance.ID)
		requestContext := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		requestContext = context.WithValue(requestContext, principalContextKey, Principal{User: owner})
		req = req.WithContext(requestContext)
		recorder := httptest.NewRecorder()
		server.handleInstanceFiles(recorder, req)
		return recorder
	}

	loading := request()
	if loading.Code != http.StatusAccepted {
		t.Fatalf("initial file list status=%d body=%s", loading.Code, loading.Body.String())
	}
	var pending struct {
		Status       string `json:"status"`
		RefreshJobID string `json:"refresh_job_id"`
	}
	if err := json.Unmarshal(loading.Body.Bytes(), &pending); err != nil || pending.Status != "loading" || pending.RefreshJobID == "" {
		t.Fatalf("initial file list response=%#v err=%v", pending, err)
	}
	result := `{"files":[{"name":"addons","path":"left4dead2/addons","is_dir":true,"size":0,"modified_at":"2026-08-12T00:00:00Z"}],"shortcuts":[{"name":"admins_simple.ini","path":"left4dead2/addons/sourcemod/configs/admins_simple.ini","is_dir":false,"size":12,"modified_at":"2026-08-12T00:00:00Z"}]}`
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded',progress=100,result=?2,finished_at=?3 WHERE id=?1`, pending.RefreshJobID, result, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	ready := request()
	if ready.Code != http.StatusOK {
		t.Fatalf("completed file list status=%d body=%s", ready.Code, ready.Body.String())
	}
	var completed struct {
		Status    string           `json:"status"`
		Items     []map[string]any `json:"items"`
		Shortcuts []map[string]any `json:"shortcuts"`
	}
	if err := json.Unmarshal(ready.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" || len(completed.Items) != 1 || completed.Items[0]["type"] != "directory" ||
		len(completed.Shortcuts) != 1 || completed.Shortcuts[0]["path"] != "left4dead2/addons/sourcemod/configs/admins_simple.ini" {
		t.Fatalf("completed file list response=%#v", completed)
	}
	var requestedBy string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(requested_by,'') FROM jobs WHERE id=?1`, pending.RefreshJobID).Scan(&requestedBy); err != nil {
		t.Fatal(err)
	}
	if requestedBy != "" {
		t.Fatalf("file list refresh requester=%q, want system background task", requestedBy)
	}
}

func TestPackageNameDoesNotKeepSignedURLQuery(t *testing.T) {
	if got := packageNameFromSource("https://mods.example/plugin.zip?token=secret#fragment"); got != "plugin.zip" {
		t.Fatalf("packageNameFromSource() = %q", got)
	}
}

func TestNormalizePlayerBanSupportsTimedAndPermanentBans(t *testing.T) {
	command, err := normalizePlayerCommand("ban_player", map[string]any{"steam_id": "STEAM_1:1:12345", "duration_minutes": 60, "reason": "spam"})
	if err != nil {
		t.Fatal(err)
	}
	if command != "banid 60 STEAM_1:1:12345 kick; writeid" {
		t.Fatalf("unexpected command %q", command)
	}
	for _, payload := range []map[string]any{
		{"steam_id": "bad;quit"},
		{"steam_id": "STEAM_1:1:12345", "duration_minutes": -1},
		{"steam_id": "STEAM_1:1:12345", "duration_minutes": 525601},
		{"steam_id": "STEAM_1:1:12345", "reason": "line\nbreak"},
	} {
		if _, err := normalizePlayerCommand("ban_player", payload); err == nil {
			t.Fatalf("unsafe ban payload was accepted: %#v", payload)
		}
	}
}
