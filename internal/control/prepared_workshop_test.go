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

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func readyPreparedWorkshop(t *testing.T, s *Store) (domain.Job, domain.PreparedWorkshop) {
	t.Helper()
	job, _, err := s.QueueInstanceJob(context.Background(), "instance", "prepare_package", map[string]any{"kind": domain.PreparedWorkshopKind, "name": domain.PreparedWorkshopName, "source": "123"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := domain.PreparedWorkshop{PreparedPackage: domain.PreparedPackage{ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Kind: domain.PreparedWorkshopKind, Name: domain.PreparedWorkshopName, ExpiresAt: time.Now().UTC().Add(time.Hour), Size: 200}, SourceID: "123", SourceIDs: []string{"10", "20"}, Collection: true, Files: []domain.PreparedWorkshopFile{{SourceID: "10", Name: "10_one.vpk", SHA256: strings.Repeat("a", 64), Size: 100}, {SourceID: "20", Name: "20_two.vpk", SHA256: strings.Repeat("b", 64), Size: 100}}}
	bundle.SHA256 = bundle.Digest()
	raw, _ := json.Marshal(bundle)
	_ = json.Unmarshal(raw, &job.Result)
	job, err = s.UpdateJob(context.Background(), job.ID, domain.JobSucceeded, 100, "ready", job.Result)
	if err != nil {
		t.Fatal(err)
	}
	return job, bundle
}

func workshopProjection(t *testing.T, job domain.Job, bundle domain.PreparedWorkshop) domain.Job {
	t.Helper()
	job.Status = domain.JobSucceeded
	files := []any{}
	for _, file := range bundle.Files {
		target, err := file.Target()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, map[string]any{"source_id": file.SourceID, "path": target, "sha256": file.SHA256, "size": file.Size, "version": file.SHA256[:12]})
	}
	job.Result = map[string]any{"kind": "workshop", "source_id": bundle.SourceID, "source_ids": bundle.SourceIDs, "collection": bundle.Collection, "files": files}
	return job
}

func TestPreparedWorkshopQueueProjectionAndOwnership(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	p := installQueuePrincipal(t, s)
	prepared, bundle := readyPreparedWorkshop(t, s)
	queue := submitInstallQueue(t, s, p, prepared.ID)
	if changed, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, true, false); err != nil || changed {
		t.Fatalf("old Agent created bundle writer: %v %v", changed, err)
	}
	if child := queueChild(t, s, queue.ID, 0); child.ID != "" {
		t.Fatal("incompatible Agent holds write barrier")
	}
	if _, err := s.advancePreparedInstallQueue(ctx, queue.ID, true, false, true); err != nil {
		t.Fatal(err)
	}
	child := queueChild(t, s, queue.ID, 0)
	if child.Payload["kind"] != domain.PreparedWorkshopKind || child.Payload["expected_sha256"] != bundle.SHA256 {
		t.Fatal("bundle metadata lost")
	}
	projected := workshopProjection(t, child, bundle)
	if err := s.ApplyJobResult(ctx, projected); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyJobResult(ctx, projected); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM addon_packages WHERE instance_id='instance' AND kind='workshop'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("projection count=%d err=%v", count, err)
	}
}

func TestPreparedWorkshopRejectsForgedProjectionAndMetadata(t *testing.T) {
	for _, scenario := range []string{"path", "hash", "count", "size", "fractional-size", "manifest", "source", "members", "file-source", "version", "kind"} {
		t.Run(scenario, func(t *testing.T) {
			s := packageRenameStore(t)
			ctx := context.Background()
			prepared, bundle := readyPreparedWorkshop(t, s)
			child, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": prepared.ID}, "", "")
			if err != nil {
				t.Fatal(err)
			}
			projected := workshopProjection(t, child, bundle)
			files := projected.Result["files"].([]any)
			switch scenario {
			case "source":
				projected.Result["source_id"] = "999"
			case "members":
				projected.Result["source_ids"] = []string{"20", "10"}
			case "file-source":
				files[0].(map[string]any)["source_id"] = "20"
			case "version":
				files[0].(map[string]any)["version"] = "forged"
			case "kind":
				projected.Result["kind"] = "vpk"
			case "path":
				files[0].(map[string]any)["path"] = "left4dead2/addons/other.vpk"
			case "hash":
				files[0].(map[string]any)["sha256"] = strings.Repeat("c", 64)
			case "size":
				files[0].(map[string]any)["size"] = 200
			case "fractional-size":
				files[0].(map[string]any)["size"] = 100.5
			case "count":
				projected.Result["files"] = files[:1]
			case "manifest":
				if _, err := s.db.Exec(`UPDATE jobs SET result=json_set(result,'$.files[0].name','forged.vpk') WHERE id=?1`, prepared.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ApplyJobResult(ctx, projected); err == nil {
				t.Fatal("forged bundle projection accepted")
			}
			var count int
			_ = s.db.QueryRow(`SELECT count(*) FROM addon_packages WHERE kind='workshop'`).Scan(&count)
			if count != 0 {
				t.Fatal("partially projected invalid bundle")
			}
		})
	}
}

func TestPreparedWorkshopQuotaAndOwnedTarget(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	prepared, bundle := readyPreparedWorkshop(t, s)
	target, _ := bundle.Files[0].Target()
	if _, err := s.db.Exec(`INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk) VALUES('owner','instance','owned','vpk','v1','upload','','a',?1,true,'normal')`, target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "install_mod", map[string]any{"prepared_package_id": prepared.ID}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("owned path allowed: %v", err)
	}
	for i := 1; i < domain.PreparedWorkshopSlots; i++ {
		readyPreparedWorkshop(t, s)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", map[string]any{"kind": domain.PreparedWorkshopKind}, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("bundle quota bypassed: %v", err)
	}
	if _, _, err := s.QueueInstanceJob(ctx, "instance", "prepare_package", map[string]any{"kind": "vpk", "name": "single.vpk"}, "", ""); err != nil {
		t.Fatalf("bundle quota blocked single file: %v", err)
	}
}

func TestPreparedWorkshopListRestoresReadyMetadataWithoutManifest(t *testing.T) {
	s := packageRenameStore(t)
	readyPreparedWorkshop(t, s)
	server := recoveryServer(t, s)
	route := chi.NewRouteContext()
	route.URLParams.Add("instanceID", "instance")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	res := httptest.NewRecorder()
	server.handlePreparedPackages(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"ready":true`) || !strings.Contains(res.Body.String(), `"file_count":2`) || !strings.Contains(res.Body.String(), domain.PreparedWorkshopName) {
		t.Fatalf("invalid Workshop list: %d %s", res.Code, res.Body)
	}
	if strings.Contains(res.Body.String(), "10_one.vpk") || strings.Contains(res.Body.String(), `"source_ids"`) {
		t.Fatal("list exposed internal manifest")
	}
}

func TestPreparedWorkshopHTTPRejectsNonIDInput(t *testing.T) {
	for _, body := range []string{`{"kind":"workshop","source":"https://example.com/file.vpk"}`, `{"kind":"workshop","source":"123 +quit"}`, `{"kind":"workshop","source":"123","content_base64":"YQ=="}`, `{"kind":"workshop","source":"123","name":"caller.vpk"}`, `{"kind":"workshop","source":"123","source_transport":"qq_flash"}`} {
		s := packageRenameStore(t)
		server := recoveryServer(t, s)
		route := chi.NewRouteContext()
		route.URLParams.Add("instanceID", "instance")
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		res := httptest.NewRecorder()
		server.handlePreparePackage(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid request: %d %s", res.Code, res.Body)
		}
	}
}
