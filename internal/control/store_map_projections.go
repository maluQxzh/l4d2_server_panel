package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

var safeMapStartName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (s *Store) applyMapJobResult(ctx context.Context, job domain.Job) error {
	switch job.Type {
	case "map.import.qq_flash":
		return s.applyMapImportResult(ctx, job)
	case "map.assign", "map.unassign":
		return s.applyMapAssignmentResult(ctx, job)
	case "map.verify", "map.inventory":
		return s.applyMapVerificationResult(ctx, job)
	case "map.delete":
		mapID := strings.TrimSpace(stringFromAny(job.Payload["map_id"]))
		if mapID == "" || mapID != strings.TrimSpace(stringFromAny(job.Result["map_id"])) {
			return errors.New("Agent returned invalid map deletion metadata")
		}
		_, err := s.db.ExecContext(ctx, `DELETE FROM maps WHERE id=?1 AND node_id=?2`, mapID, job.NodeID)
		return err
	default:
		return nil
	}
}

func (s *Store) applyMapImportResult(ctx context.Context, job domain.Job) error {
	mapID := strings.TrimSpace(stringFromAny(job.Payload["map_id"]))
	if mapID == "" || uuid.Validate(mapID) != nil || mapID != strings.TrimSpace(stringFromAny(job.Result["map_id"])) {
		return errors.New("Agent returned an invalid map ID")
	}
	name := strings.TrimSpace(stringFromAny(job.Result["name"]))
	if !validProjectedMapText(name, 255, false) {
		return errors.New("Agent returned an invalid map name")
	}
	author := strings.TrimSpace(stringFromAny(job.Result["author"]))
	version := strings.TrimSpace(stringFromAny(job.Result["version"]))
	description := strings.TrimSpace(stringFromAny(job.Result["description"]))
	if !validProjectedMapText(author, 255, true) || !validProjectedMapText(version, 64, true) || !validProjectedMapText(description, 2048, true) {
		return errors.New("Agent returned invalid map mission metadata")
	}
	aggregateSHA := strings.ToLower(strings.TrimSpace(stringFromAny(job.Result["aggregate_sha256"])))
	if validateSHA256(aggregateSHA) != nil {
		return errors.New("Agent returned an invalid aggregate map digest")
	}
	totalSize, ok := nonNegativeInt64(job.Result["total_size"])
	if !ok || totalSize == 0 {
		return errors.New("Agent returned an invalid map size")
	}
	removedFiles := intFromAny(job.Result["removed_files"])
	removedBytes, removedBytesOK := nonNegativeInt64(job.Result["removed_bytes"])
	if removedFiles < 0 || !removedBytesOK {
		return errors.New("Agent returned invalid map cleanup statistics")
	}
	sourceName := strings.TrimSpace(stringFromAny(job.Result["source_name"]))
	if len(sourceName) > 255 {
		return errors.New("Agent returned an invalid map source name")
	}

	artifacts, err := mapArtifactsFromResult(mapID, job.Result["artifacts"])
	if err != nil {
		return err
	}
	bsps, err := mapBSPsFromResult(job.Result["bsps"])
	if err != nil {
		return err
	}
	modes, err := mapModesFromResult(job.Result["modes"])
	if err != nil {
		return err
	}
	posterMIME, posterData, err := mapPosterFromResult(job.Result["poster_mime"], job.Result["poster_base64"])
	if err != nil {
		return err
	}
	warnings, err := mapWarningsFromResult(job.Result["warnings"])
	if err != nil {
		return err
	}
	replicas, err := mapReplicasFromResult(job.Result["replicas"])
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	createdAt := jobProjectionTimestamp(job)
	if _, err := tx.ExecContext(ctx, `INSERT INTO maps(id,node_id,name,author,version,description,poster_mime,poster_data,
		health,aggregate_sha256,total_size,source_type,source_name,removed_files,removed_bytes,created_by,created_at,updated_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,'healthy',?9,?10,'qq_flash',?11,?12,?13,?14,?15,?15)
		ON CONFLICT(id) DO NOTHING`, mapID, job.NodeID, name, author, version, description, posterMIME, posterData,
		aggregateSHA, totalSize, sourceName, removedFiles, removedBytes, nullableUUID(job.RequestedBy), createdAt); err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO map_artifacts(id,map_id,ordinal,sha256,size) VALUES(?1,?2,?3,?4,?5)
			ON CONFLICT(map_id,ordinal) DO NOTHING`, artifact.ID, mapID, artifact.Ordinal, artifact.SHA256, artifact.Size); err != nil {
			return err
		}
	}
	for _, bsp := range bsps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO map_bsps(map_id,virtual_path,start_name,dictionary_present) VALUES(?1,?2,?3,?4)
			ON CONFLICT(map_id,virtual_path) DO NOTHING`, mapID, bsp.VirtualPath, bsp.StartName, bsp.DictionaryPresent); err != nil {
			return err
		}
	}
	for modeIndex, mode := range modes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO map_modes(map_id,name,ordinal) VALUES(?1,?2,?3)
			ON CONFLICT(map_id,name) DO NOTHING`, mapID, mode.Name, modeIndex); err != nil {
			return err
		}
		for _, chapter := range mode.Chapters {
			if _, err := tx.ExecContext(ctx, `INSERT INTO map_chapters(map_id,mode,ordinal,bsp_name,display_name,image)
				VALUES(?1,?2,?3,?4,?5,?6) ON CONFLICT(map_id,mode,ordinal) DO NOTHING`, mapID, mode.Name,
				chapter.Ordinal, chapter.BSPName, chapter.DisplayName, chapter.Image); err != nil {
				return err
			}
		}
	}
	for index, warning := range warnings {
		warningID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("l4d2panel:map-warning:%s:%d", mapID, index))).String()
		if _, err := tx.ExecContext(ctx, `INSERT INTO map_warnings(id,map_id,code,message,virtual_path) VALUES(?1,?2,?3,?4,?5)
			ON CONFLICT(id) DO NOTHING`, warningID, mapID, warning.Code, warning.Message, warning.VirtualPath); err != nil {
			return err
		}
	}
	for _, replica := range replicas {
		if _, err := tx.ExecContext(ctx, `INSERT INTO map_replicas(map_id,filesystem_id,health,verified_at) VALUES(?1,?2,?3,?4)
			ON CONFLICT(map_id,filesystem_id) DO UPDATE SET health=excluded.health,verified_at=excluded.verified_at`,
			mapID, replica.FilesystemID, replica.Health, createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validProjectedMapText(value string, limit int, emptyOK bool) bool {
	return (emptyOK || value != "") && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit && !strings.ContainsAny(value, "\x00\r\n")
}

func mapPosterFromResult(mimeValue, dataValue any) (string, []byte, error) {
	mime := strings.TrimSpace(stringFromAny(mimeValue))
	encoded := strings.TrimSpace(stringFromAny(dataValue))
	if mime == "" && encoded == "" {
		return "", []byte{}, nil
	}
	if mime != "image/jpeg" || encoded == "" || base64.StdEncoding.DecodedLen(len(encoded)) > 1<<20 {
		return "", nil, errors.New("Agent returned invalid map poster metadata")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return "", nil, errors.New("Agent returned invalid map poster content")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 1280 || config.Height > 720 {
		return "", nil, errors.New("Agent returned invalid map poster image")
	}
	return mime, raw, nil
}

func mapModesFromResult(value any) ([]domain.MapMode, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok || len(items) > 64 {
		return nil, errors.New("Agent returned an invalid map mode list")
	}
	result := make([]domain.MapMode, 0, len(items))
	seenModes := make(map[string]bool)
	totalChapters := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("Agent returned invalid map mode metadata")
		}
		name := strings.ToLower(strings.TrimSpace(stringFromAny(item["name"])))
		chaptersRaw, ok := item["chapters"].([]any)
		if !safeMapStartName.MatchString(name) || len(name) > 64 || seenModes[name] || !ok || len(chaptersRaw) == 0 || len(chaptersRaw) > 4096 {
			return nil, errors.New("Agent returned invalid map mode metadata")
		}
		seenModes[name] = true
		mode := domain.MapMode{Name: name}
		seenOrdinals := make(map[int]bool)
		for _, chapterRaw := range chaptersRaw {
			chapterMap, ok := chapterRaw.(map[string]any)
			if !ok {
				return nil, errors.New("Agent returned invalid map chapter metadata")
			}
			chapter := domain.MapChapter{Ordinal: intFromAny(chapterMap["ordinal"]),
				BSPName:     strings.TrimSpace(stringFromAny(chapterMap["bsp_name"])),
				DisplayName: strings.TrimSpace(stringFromAny(chapterMap["display_name"])),
				Image:       strings.TrimSpace(stringFromAny(chapterMap["image"]))}
			if chapter.Ordinal < 1 || chapter.Ordinal > 4096 || seenOrdinals[chapter.Ordinal] || !safeMapStartName.MatchString(chapter.BSPName) ||
				!validProjectedMapText(chapter.DisplayName, 255, false) || !validProjectedMapText(chapter.Image, 512, true) {
				return nil, errors.New("Agent returned invalid map chapter metadata")
			}
			seenOrdinals[chapter.Ordinal] = true
			mode.Chapters = append(mode.Chapters, chapter)
			totalChapters++
		}
		result = append(result, mode)
	}
	if totalChapters > 4096 {
		return nil, errors.New("Agent returned too many map chapters")
	}
	return result, nil
}

func (s *Store) applyMapAssignmentResult(ctx context.Context, job domain.Job) error {
	mapID := strings.TrimSpace(stringFromAny(job.Payload["map_id"]))
	if mapID == "" || mapID != strings.TrimSpace(stringFromAny(job.Result["map_id"])) || job.InstanceID == "" {
		return errors.New("Agent returned invalid map assignment metadata")
	}
	if resultInstance := strings.TrimSpace(stringFromAny(job.Result["instance_id"])); resultInstance != job.InstanceID {
		return errors.New("Agent returned an invalid map assignment instance")
	}
	if job.Type == "map.unassign" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM map_assignments WHERE map_id=?1 AND instance_id=?2`, mapID, job.InstanceID)
		return err
	}
	restartRequired, _ := job.Result["restart_required"].(bool)
	projectedAt := jobProjectionTimestamp(job)
	_, err := s.db.ExecContext(ctx, `INSERT INTO map_assignments(map_id,instance_id,restart_required,created_at,updated_at)
		VALUES(?1,?2,?3,?4,?4) ON CONFLICT(map_id,instance_id) DO UPDATE SET
		restart_required=excluded.restart_required,updated_at=excluded.updated_at`, mapID, job.InstanceID, restartRequired, projectedAt)
	return err
}

func (s *Store) applyMapVerificationResult(ctx context.Context, job domain.Job) error {
	mapID := strings.TrimSpace(stringFromAny(job.Payload["map_id"]))
	if mapID == "" || mapID != strings.TrimSpace(stringFromAny(job.Result["map_id"])) {
		return errors.New("Agent returned invalid map verification metadata")
	}
	health := strings.TrimSpace(stringFromAny(job.Result["health"]))
	if health != string(domain.MapHealthy) && health != string(domain.MapDegraded) && health != string(domain.MapBroken) {
		return errors.New("Agent returned invalid map health")
	}
	replicas, err := mapReplicasFromResult(job.Result["replicas"])
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	verifiedAt := jobProjectionTimestamp(job)
	if _, err := tx.ExecContext(ctx, `UPDATE maps SET health=?2,updated_at=?3 WHERE id=?1 AND node_id=?4`, mapID, health, verifiedAt, job.NodeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM map_replicas WHERE map_id=?1`, mapID); err != nil {
		return err
	}
	for _, replica := range replicas {
		if _, err := tx.ExecContext(ctx, `INSERT INTO map_replicas(map_id,filesystem_id,health,verified_at) VALUES(?1,?2,?3,?4)`,
			mapID, replica.FilesystemID, replica.Health, verifiedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func mapArtifactsFromResult(mapID string, value any) ([]domain.MapArtifact, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 || len(items) > 64 {
		return nil, errors.New("Agent returned an invalid map artifact list")
	}
	result := make([]domain.MapArtifact, 0, len(items))
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("Agent returned an invalid map artifact")
		}
		sha := strings.ToLower(strings.TrimSpace(stringFromAny(item["sha256"])))
		size, sizeOK := nonNegativeInt64(item["size"])
		if validateSHA256(sha) != nil || !sizeOK || size == 0 || intFromAny(item["ordinal"]) != index {
			return nil, errors.New("Agent returned invalid map artifact metadata")
		}
		result = append(result, domain.MapArtifact{ID: uuid.NewSHA1(uuid.NameSpaceOID,
			[]byte(fmt.Sprintf("l4d2panel:map-artifact:%s:%d", mapID, index))).String(), MapID: mapID, Ordinal: index, SHA256: sha, Size: size})
	}
	return result, nil
}

func mapBSPsFromResult(value any) ([]domain.MapBSP, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 || len(items) > 4096 {
		return nil, errors.New("Agent returned an invalid BSP list")
	}
	result := make([]domain.MapBSP, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("Agent returned invalid BSP metadata")
		}
		virtualPath := strings.TrimSpace(stringFromAny(item["virtual_path"]))
		startName := strings.TrimSpace(stringFromAny(item["start_name"]))
		key := strings.ToLower(filepath.ToSlash(virtualPath))
		dictionaryPresent, dictionaryOK := item["dictionary_present"].(bool)
		if !dictionaryOK || virtualPath == "" || len(virtualPath) > 512 || seen[key] ||
			(!strings.HasPrefix(key, "maps/") || !strings.HasSuffix(key, ".bsp")) ||
			(startName != "" && !safeMapStartName.MatchString(startName)) {
			return nil, errors.New("Agent returned invalid BSP metadata")
		}
		seen[key] = true
		result = append(result, domain.MapBSP{VirtualPath: filepath.ToSlash(virtualPath), StartName: startName, DictionaryPresent: dictionaryPresent})
	}
	return result, nil
}

func mapWarningsFromResult(value any) ([]domain.MapWarning, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok || len(items) > 4096 {
		return nil, errors.New("Agent returned an invalid map warning list")
	}
	result := make([]domain.MapWarning, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("Agent returned invalid map warning metadata")
		}
		warning := domain.MapWarning{Code: strings.TrimSpace(stringFromAny(item["code"])),
			Message: strings.TrimSpace(stringFromAny(item["message"])), VirtualPath: strings.TrimSpace(stringFromAny(item["virtual_path"]))}
		if warning.Code == "" || len(warning.Code) > 64 || warning.Message == "" || len(warning.Message) > 1024 || len(warning.VirtualPath) > 512 {
			return nil, errors.New("Agent returned invalid map warning metadata")
		}
		result = append(result, warning)
	}
	return result, nil
}

func mapReplicasFromResult(value any) ([]domain.MapReplica, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 || len(items) > 64 {
		return nil, errors.New("Agent returned an invalid map replica list")
	}
	result := make([]domain.MapReplica, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("Agent returned invalid map replica metadata")
		}
		filesystemID := strings.TrimSpace(stringFromAny(item["filesystem_id"]))
		health := strings.TrimSpace(stringFromAny(item["health"]))
		if filesystemID == "" || len(filesystemID) > 128 || seen[filesystemID] || (health != "healthy" && health != "missing" && health != "corrupt") {
			return nil, errors.New("Agent returned invalid map replica metadata")
		}
		seen[filesystemID] = true
		result = append(result, domain.MapReplica{FilesystemID: filesystemID, Health: health})
	}
	return result, nil
}

func nonNegativeInt64(value any) (int64, bool) {
	var result int64
	switch typed := value.(type) {
	case int:
		result = int64(typed)
	case int32:
		result = int64(typed)
	case int64:
		result = typed
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt64 {
			return 0, false
		}
		result = int64(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		result = parsed
	default:
		return 0, false
	}
	return result, result >= 0
}
