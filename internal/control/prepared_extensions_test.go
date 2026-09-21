package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func readyPreparedExtension(t *testing.T, s *Store, kind string) domain.Job {
	t.Helper()
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "prepare_package", map[string]any{"kind": kind, "name": "extension.zip"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	meta := domain.PreparedPackage{ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Kind: kind, Name: "extension.zip", SHA256: strings.Repeat("a", 64), Size: 100, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	raw, _ := json.Marshal(meta)
	_ = json.Unmarshal(raw, &job.Result)
	job, err = s.UpdateJob(context.Background(), job.ID, domain.JobSucceeded, 100, "ready", job.Result)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPreparedExtensionQueueRechecksDependency(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	p := installQueuePrincipal(t, s)
	prepared := readyPreparedExtension(t, s, "sourcemod")
	payload := map[string]any{"prepared_package_id": prepared.ID, "acknowledge_native_risk": true}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", payload, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing dependency: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO addon_packages(id,instance_id,name,kind,enabled) VALUES('meta','instance','Metamod','metamod',true)`); err != nil {
		t.Fatal(err)
	}
	request := preparedInstallQueueRequest{IDs: []string{prepared.ID}, Risk: true}
	queue, _, err := s.QueuePreparedInstalls(ctx, "instance", request, p, "batch", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE addon_packages SET enabled=false WHERE id='meta'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetJob(ctx, queue.ID)
	if err != nil || stored.Status != domain.JobFailed {
		t.Fatalf("dependency drift: %v %v", stored.Status, err)
	}
	if queueChild(t, s, queue.ID, 0).ID != "" {
		t.Fatal("created writer with missing dependency")
	}
}

func TestPreparedExtensionQueueInventoryCapabilityAndProjection(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	p := installQueuePrincipal(t, s)
	prepared := readyPreparedExtension(t, s, "metamod")
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": prepared.ID}, "", ""); err == nil {
		t.Fatal("risk not required")
	}
	queue, _, err := s.QueuePreparedInstalls(ctx, "instance", preparedInstallQueueRequest{IDs: []string{prepared.ID}, Risk: true}, p, "batch", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true, true); err != nil || changed {
		t.Fatal("old Agent advanced", changed, err)
	}
	if queueChild(t, s, queue.ID, 0).ID != "" {
		t.Fatal("old Agent acquired barrier")
	}
	// Ownership may change while this coordinator waits without a write lock.
	if _, err := s.db.Exec(`INSERT INTO addon_packages(id,instance_id,name,kind,enabled,path) VALUES('other','instance','Other','plugin',true,'left4dead2/addons/sourcemod/plugins/other.smx')`); err != nil {
		t.Fatal(err)
	}
	const componentID = "b5031653-a35f-4a03-bedd-1c0f0cf084e0"
	if _, err := s.db.Exec(`INSERT INTO addon_packages(id,instance_id,name,kind,enabled) VALUES(?1,'instance','Metamod','metamod',true)`, componentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size) VALUES(?1,'instance','left4dead2/addons/metamod/bin/old.dll',?2,7)`, componentID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	child := queueChild(t, s, queue.ID, 0)
	paths, ok := child.Payload["registered_paths"].(map[string]any)
	if !ok || paths["left4dead2/addons/sourcemod/plugins/other.smx"] != "other" {
		t.Fatal("stale ownership", child.Payload)
	}
	files, ok := child.Payload["managed_files"].([]any)
	if child.Payload["package_id"] != componentID || !ok || len(files) != 1 || files[0].(map[string]any)["path"] != "left4dead2/addons/metamod/bin/old.dll" {
		t.Fatal("current component inventory not included", child.Payload)
	}
	child.Status = domain.JobSucceeded
	child.Result = map[string]any{"kind": "metamod", "sha256": strings.Repeat("a", 64), "size": 100, "verified": true, "verification_command": "meta version", "managed_files": []any{map[string]any{"path": "left4dead2/addons/metamod/bin/server.dll", "sha256": strings.Repeat("b", 64), "size": 10}}}
	child.Result["package_id"] = componentID
	for _, field := range []string{"kind", "sha256", "size", "package_id", "verified", "verification_command"} {
		forged := child
		forged.Result = cloneMap(child.Result)
		forged.Result[field] = "forged"
		if field == "size" {
			forged.Result[field] = 100.5
		}
		if err := s.ApplyJobResult(ctx, forged); err == nil {
			t.Fatalf("forged %s accepted", field)
		}
	}
	for range 2 {
		if err := s.ApplyJobResult(ctx, child); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM addon_packages WHERE instance_id='instance' AND kind='metamod'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("non-idempotent projection", count, err)
	}
}
