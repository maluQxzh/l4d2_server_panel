package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestMapImportProjectsMissionMetadataPosterModesAndChapters(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	nodeID, mapID, userID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role)
		VALUES(?2,'map-uploader','map-uploader','地图上传者','hash','operator');
		INSERT INTO nodes(id,name,status) VALUES(?1,'map-node','online')`, nodeID, userID); err != nil {
		t.Fatal(err)
	}
	posterImage := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			posterImage.SetRGBA(x, y, color.RGBA{R: 30, G: 60, B: 90, A: 255})
		}
	}
	var poster bytes.Buffer
	if err := jpeg.Encode(&poster, posterImage, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := domain.Job{ID: uuid.NewString(), NodeID: nodeID, Type: "map.import.qq_flash", Status: domain.JobSucceeded,
		Payload: map[string]any{"map_id": mapID}, RequestedBy: userID, CreatedAt: now, FinishedAt: &now,
		Result: map[string]any{
			"map_id": mapID, "name": "Back to School", "author": "RedRogue", "version": "1.05", "description": "Return to class.",
			"aggregate_sha256": strings.Repeat("a", 64), "total_size": float64(10), "source_name": "bts.vpk",
			"removed_files": float64(0), "removed_bytes": float64(0), "poster_mime": "image/jpeg",
			"poster_base64": base64.StdEncoding.EncodeToString(poster.Bytes()),
			"artifacts":     []any{map[string]any{"ordinal": float64(0), "sha256": strings.Repeat("b", 64), "size": float64(10)}},
			"bsps": []any{
				map[string]any{"virtual_path": "maps/bts01.bsp", "start_name": "bts01", "dictionary_present": true},
				map[string]any{"virtual_path": "maps/bts02.bsp", "start_name": "bts02", "dictionary_present": true},
			},
			"modes": []any{map[string]any{"name": "coop", "chapters": []any{
				map[string]any{"ordinal": float64(1), "bsp_name": "bts01", "display_name": "学校外围", "image": "maps/bts01"},
				map[string]any{"ordinal": float64(2), "bsp_name": "bts02", "display_name": "城镇", "image": "maps/bts02"},
			}}},
			"warnings": []any{}, "replicas": []any{map[string]any{"filesystem_id": "volume-1", "health": "healthy"}},
		},
	}
	if err := store.ApplyJobResult(ctx, job); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetMap(ctx, mapID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Author != "RedRogue" || item.Version != "1.05" || item.PosterURL == "" || len(item.Modes) != 1 || len(item.Modes[0].Chapters) != 2 || len(item.BSPs) != 2 ||
		item.CreatedBy != userID || item.CreatedByName != "地图上传者" {
		t.Fatalf("unexpected projected map: %+v", item)
	}
	posterNode, mime, body, err := store.GetMapPoster(ctx, mapID)
	if err != nil || posterNode != nodeID || mime != "image/jpeg" || !bytes.Equal(body, poster.Bytes()) {
		t.Fatalf("unexpected poster projection: node=%q mime=%q bytes=%d err=%v", posterNode, mime, len(body), err)
	}

	duplicateID := uuid.NewString()
	duplicateJob := job
	duplicateJob.ID = uuid.NewString()
	duplicateJob.Payload = map[string]any{"map_id": duplicateID}
	duplicateJob.Result = map[string]any{
		"map_id": mapID, "name": "Back to School (same content)", "aggregate_sha256": strings.Repeat("a", 64),
		"total_size": float64(10), "source_name": "second-source.vpk", "removed_files": float64(0), "removed_bytes": float64(0),
		"artifacts": []any{map[string]any{"ordinal": float64(0), "sha256": strings.Repeat("b", 64), "size": float64(10)}},
		"bsps":      []any{map[string]any{"virtual_path": "maps/bts01.bsp", "start_name": "bts01", "dictionary_present": true}},
		"modes":     []any{map[string]any{"name": "coop", "chapters": []any{map[string]any{"ordinal": float64(1), "bsp_name": "bts01", "display_name": "学校外围"}}}},
		"warnings":  []any{}, "replicas": []any{map[string]any{"filesystem_id": "volume-1", "health": "healthy"}},
	}
	if err := store.ApplyJobResult(ctx, duplicateJob); err != nil {
		t.Fatal(err)
	}
	var mapCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM maps WHERE node_id=?1`, nodeID).Scan(&mapCount); err != nil {
		t.Fatal(err)
	}
	if mapCount != 1 {
		t.Fatalf("duplicate map import created %d logical records", mapCount)
	}
}
