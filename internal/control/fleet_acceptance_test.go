package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// TestFleetScaleAcceptance is an explicit, PostgreSQL-backed control-plane
// acceptance harness. It deliberately does not claim SteamCMD or game-server
// coverage: simulated nodes never connect an Agent. Enable it in CI or locally
// with RUN_FLEET_ACCEPTANCE=1 and TEST_DATABASE_URL.
func TestFleetScaleAcceptance(t *testing.T) {
	if os.Getenv("RUN_FLEET_ACCEPTANCE") != "1" {
		t.Skip("RUN_FLEET_ACCEPTANCE=1 is required")
	}
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "fleet_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`) }()

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := OpenStore(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	password := "fleet-acceptance-password"
	passwordHash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.BootstrapOwner(ctx, "fleet-owner", "Fleet Owner", passwordHash); err != nil || !created {
		t.Fatalf("bootstrap fleet owner: created=%v err=%v", created, err)
	}

	server := NewServer(Config{MasterKey: bytes.Repeat([]byte{0x5a}, 32), CookieSecure: false, SessionTTL: time.Hour, Timezone: "UTC"},
		store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	client := &fleetAPIClient{baseURL: httpServer.URL, client: &http.Client{Jar: jar, Timeout: 20 * time.Second}}
	login, err := client.request(http.MethodPost, "/api/v1/auth/login", map[string]any{"username": "fleet-owner", "password": password}, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	client.csrf, _ = login["csrf_token"].(string)
	if client.csrf == "" {
		t.Fatal("login did not return a CSRF token")
	}

	const nodeCount, instanceCount = 20, 100
	nodeIDs := make([]string, nodeCount)
	for index := 0; index < nodeCount; index++ {
		issued, err := client.request(http.MethodPost, "/api/v1/nodes/enrollment-tokens",
			map[string]any{"label": fmt.Sprintf("fleet-node-%02d", index), "expires_in_minutes": 30}, "", http.StatusCreated)
		if err != nil {
			t.Fatal(err)
		}
		enrolled, err := client.request(http.MethodPost, "/api/v1/agent/enroll", map[string]any{
			"token": issued["token"], "name": fmt.Sprintf("fleet-node-%02d", index), "os": "linux", "arch": "amd64",
			"agent_version": "fleet-simulator", "instance_root": fmt.Sprintf("/srv/l4d2/fleet-%02d", index),
		}, "", http.StatusCreated)
		if err != nil {
			t.Fatal(err)
		}
		nodeIDs[index], _ = enrolled["node_id"].(string)
	}

	type createdInstance struct{ instanceID, jobID string }
	created := make([]createdInstance, instanceCount)
	errorsFound := make(chan error, nodeCount)
	var workers sync.WaitGroup
	for nodeIndex, nodeID := range nodeIDs {
		workers.Add(1)
		go func(nodeIndex int, nodeID string) {
			defer workers.Done()
			for offset := 0; offset < instanceCount/nodeCount; offset++ {
				index := nodeIndex*(instanceCount/nodeCount) + offset
				response, requestErr := client.request(http.MethodPost, "/api/v1/instances", fleetInstanceRequest(nodeID, index),
					fmt.Sprintf("fleet:create:%03d", index), http.StatusAccepted)
				if requestErr != nil {
					errorsFound <- requestErr
					return
				}
				instance, _ := response["instance"].(map[string]any)
				created[index] = createdInstance{instanceID: stringFromAny(instance["id"]), jobID: stringFromAny(response["job_id"])}
			}
		}(nodeIndex, nodeID)
	}
	workers.Wait()
	close(errorsFound)
	for workerErr := range errorsFound {
		t.Fatal(workerErr)
	}

	// Replay a sample of create requests with the same key and body. They must
	// return the original resources and cannot grow the job or instance tables.
	for index := 0; index < 10; index++ {
		replayed, err := client.request(http.MethodPost, "/api/v1/instances", fleetInstanceRequest(nodeIDs[index/(instanceCount/nodeCount)], index),
			fmt.Sprintf("fleet:create:%03d", index), http.StatusAccepted)
		if err != nil {
			t.Fatal(err)
		}
		instance, _ := replayed["instance"].(map[string]any)
		if replayed["idempotent_replay"] != true || stringFromAny(replayed["job_id"]) != created[index].jobID || stringFromAny(instance["id"]) != created[index].instanceID {
			t.Fatalf("create replay %d was not equivalent: %#v", index, replayed)
		}
	}
	for table, expected := range map[string]int{"nodes": nodeCount, "instances": instanceCount, "jobs": instanceCount, "schedules": instanceCount} {
		var actual int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&actual); err != nil || actual != expected {
			t.Fatalf("%s count=%d want=%d err=%v", table, actual, expected, err)
		}
	}

	lastCursor := readFleetReplay(t, client, 0, nodeCount+instanceCount, "")
	if _, err := client.request(http.MethodPatch, "/api/v1/nodes/"+nodeIDs[0], map[string]any{"name": "fleet-node-renamed"}, "", http.StatusOK); err != nil {
		t.Fatal(err)
	}
	readFleetReplay(t, client, lastCursor, 1, "node.updated")
}

type fleetAPIClient struct {
	baseURL string
	client  *http.Client
	csrf    string
}

func (c *fleetAPIClient) request(method, path string, body any, idempotencyKey string, expectedStatus int) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		request.Header.Set("X-CSRF-Token", c.csrf)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseRaw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != expectedStatus {
		return nil, fmt.Errorf("%s %s returned %d: %s", method, path, response.StatusCode, responseRaw)
	}
	result := make(map[string]any)
	if len(responseRaw) > 0 {
		if err := json.Unmarshal(responseRaw, &result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func fleetInstanceRequest(nodeID string, index int) map[string]any {
	name := fmt.Sprintf("fleet-instance-%03d", index)
	return map[string]any{
		"node_id": nodeID, "name": name, "operation": "install", "host": "0.0.0.0", "port": 0,
		"config": map[string]any{"hostname": name, "mode": "coop", "difficulty": "normal", "max_players": 8,
			"start_map": "c1m1_hotel", "region": "255", "public_status": false, "auto_restart": true},
	}
}

func readFleetReplay(t *testing.T, client *fleetAPIClient, cursor int64, expected int, expectedType string) int64 {
	t.Helper()
	parsed, err := url.Parse(client.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Origin": []string{client.baseURL}}
	for _, cookie := range client.client.Jar.Cookies(parsed) {
		header.Add("Cookie", cookie.String())
	}
	wsURL := "ws" + strings.TrimPrefix(client.baseURL, "http") + fmt.Sprintf("/api/v1/events?cursor=%d", cursor)
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(15 * time.Second))
	last := cursor
	for received := 0; received < expected; {
		var event Event
		if err := connection.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event.ID <= last {
			t.Fatalf("event cursor did not advance: previous=%d event=%#v", last, event)
		}
		last = event.ID
		if expectedType == "" || event.Type == expectedType {
			received++
		}
	}
	return last
}
