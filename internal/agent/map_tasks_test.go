package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestAssignMapRefreshesRunningInstanceWithFixedRCONCommand(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	instanceRoot := filepath.Join(base, "instances", "server")
	if err := os.MkdirAll(filepath.Join(instanceRoot, "left4dead2", "addons"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := NewPathGuard([]string{filepath.Join(base, "instances")})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	processes, err := NewProcessManager(dataDir, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{DataDir: dataDir, InstanceRoots: paths.Roots(), MapRoots: []string{filepath.Join(dataDir, "maps")}}
	executor := NewExecutor(config, paths, registry, processes, log.New(io.Discard, "", 0))
	if executor.mapStoreErr != nil {
		t.Fatal(executor.mapStoreErr)
	}

	object := filepath.Join(base, "map.vpk")
	if err := os.WriteFile(object, []byte("hot-reload-map"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, size, err := hashFile(object)
	if err != nil {
		t.Fatal(err)
	}
	mapID := uuid.NewString()
	if _, err := executor.maps.publish(mapLocalRecord{
		ID: mapID, Name: "Hot reload", AggregateSHA256: sha, TotalSize: size,
		Artifacts:   []mapLocalArtifact{{Ordinal: 0, SHA256: sha, Size: size}},
		BSPs:        []mapLocalBSP{{VirtualPath: "maps/hot_reload.bsp", StartName: "hot_reload"}},
		Assignments: make(map[string]mapLocalAssignment),
	}, []string{object}); err != nil {
		t.Fatal(err)
	}

	instanceID := uuid.NewString()
	if err := registry.Put(InstanceRecord{ID: instanceID, RelativePath: instanceRoot, ExternalPID: 42, RCONHost: "127.0.0.1", RCONPort: 27015}); err != nil {
		t.Fatal(err)
	}
	var gotPassword, gotCommand string
	var gotTimeout time.Duration
	executor.mapReloadRCON = func(_ context.Context, _ InstanceRecord, _ operationPayload, password, command string, timeout time.Duration) (string, error) {
		gotPassword, gotCommand, gotTimeout = password, command, timeout
		return "refreshed", nil
	}
	job := domain.Job{ID: uuid.NewString(), InstanceID: instanceID, Type: "map.assign", Payload: map[string]any{"map_id": mapID, "rcon_password": "secret"}}
	result, err := executor.executeMapTask(context.Background(), job.Type, job, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if gotPassword != "secret" || gotCommand != "sm_update_vpk" || gotTimeout != 8*time.Second {
		t.Fatalf("RCON refresh = password %q command %q timeout %s", gotPassword, gotCommand, gotTimeout)
	}
	if hotReloaded, _ := result["hot_reloaded"].(bool); !hotReloaded {
		t.Fatalf("hot reload result = %#v", result)
	}
	if restartRequired, _ := result["restart_required"].(bool); restartRequired {
		t.Fatalf("assignment unexpectedly requires restart: %#v", result)
	}

	job.ID = uuid.NewString()
	job.Payload = map[string]any{"map_id": mapID}
	result, err = executor.executeMapTask(context.Background(), job.Type, job, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["hot_reload_skipped"] != "rcon_not_configured" {
		t.Fatalf("missing RCON result = %#v", result)
	}
	if result["restart_required"] != true {
		t.Fatal("unrefreshed assignment must remain visible as pending")
	}
	for _, reloadErr := range []error{errors.New("secret RCON transport failure"), context.Canceled, context.DeadlineExceeded} {
		executor.mapReloadRCON = func(context.Context, InstanceRecord, operationPayload, string, string, time.Duration) (string, error) {
			return "", reloadErr
		}
		job.Payload["rcon_password"] = "secret"
		result, err = executor.executeMapTask(context.Background(), job.Type, job, func(int, string) {})
		if err != nil || result["hot_reload_skipped"] != "rcon_refresh_failed" || result["restart_required"] != true {
			t.Fatalf("committed assignment lost on %v: result=%#v err=%v", reloadErr, result, err)
		}
		raw, _ := json.Marshal(result)
		if strings.Contains(string(raw), "secret") {
			t.Fatal("RCON error leaked into result")
		}
		if status, _ := executionCompletion(err); status != domain.JobSucceeded {
			t.Fatal("committed assignment was classified as failure")
		}
	}
	verifyJob := domain.Job{Type: "map.verify", Payload: map[string]any{"map_id": mapID}}
	result, err = executor.executeMapTask(context.Background(), verifyJob.Type, verifyJob, func(int, string) {})
	if err != nil || !reflect.DeepEqual(result["assignment_instance_ids"], []string{instanceID}) {
		t.Fatalf("verification lost local assignment: %#v %v", result, err)
	}
	// Reopen the registry: recovery must use durable state, not only memory.
	executor.maps.registry, err = openMapRegistry(executor.maps.defaultRoot().Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(InstanceRecord{ID: instanceID, RelativePath: instanceRoot}); err != nil {
		t.Fatal(err)
	}
	job.Type = "map.unassign"
	if _, err := executor.executeMapTask(context.Background(), job.Type, job, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	result, err = executor.executeMapTask(context.Background(), verifyJob.Type, verifyJob, func(int, string) {})
	if err != nil || !reflect.DeepEqual(result["assignment_instance_ids"], []string{}) {
		t.Fatalf("empty snapshot must be explicit: %#v %v", result, err)
	}
	if err := executor.maps.delete(mapID); err != nil {
		t.Fatal(err)
	}
	verifyJob.Type = "map.inventory"
	result, err = executor.executeMapTask(context.Background(), verifyJob.Type, verifyJob, func(int, string) {})
	if _, present := result["assignment_instance_ids"]; err != nil || present {
		t.Fatalf("missing registry must not clear assignments: %#v %v", result, err)
	}
}
