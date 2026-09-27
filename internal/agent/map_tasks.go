package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) executeMapTask(ctx context.Context, action string, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	if e.maps == nil {
		if e.mapStoreErr != nil {
			return nil, fmt.Errorf("initialize map library: %w", e.mapStoreErr)
		}
		return nil, errors.New("map library is unavailable")
	}
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	mapID := strings.TrimSpace(payload.MapID)
	if uuid.Validate(mapID) != nil {
		return nil, errors.New("map_id is invalid")
	}
	switch action {
	case "map.import.qq.flash":
		return e.importQQFlashMap(ctx, job, payload, progress)
	case "map.assign":
		return e.assignMap(ctx, job, mapID, payload, progress)
	case "map.unassign":
		return e.unassignMap(job, mapID, progress)
	case "map.verify", "map.inventory":
		progress(20, "verifying map objects and instance links")
		if action == "map.inventory" {
			if _, ok := e.maps.registry.get(mapID); !ok {
				progress(100, "map is missing from the node registry")
				return map[string]any{"map_id": mapID, "health": "broken", "replicas": []map[string]any{{
					"filesystem_id": e.maps.defaultRoot().FilesystemID, "health": "missing",
				}}}, nil
			}
		}
		health, replicas, err := e.maps.verify(mapID, e.registry, e.paths)
		if err != nil {
			return nil, err
		}
		progress(100, "map verification completed")
		return map[string]any{"map_id": mapID, "health": health, "replicas": replicas}, nil
	case "map.delete":
		progress(20, "checking map references")
		if err := e.maps.delete(mapID); err != nil {
			return nil, err
		}
		progress(100, "map permanently deleted")
		return map[string]any{"map_id": mapID}, nil
	default:
		return nil, fmt.Errorf("unsupported map task %q", action)
	}
}

func (e *Executor) importQQFlashMap(ctx context.Context, job domain.Job, payload operationPayload, progress ProgressFunc) (map[string]any, error) {
	if existing, ok := e.maps.registry.get(payload.MapID); ok {
		progress(100, "map import already committed")
		return e.maps.importResult(existing), nil
	}
	if err := validateQQFlashShareURL(payload.Source); err != nil {
		return nil, wrapMapImportFailure(mapImportStagePreflight, "", err)
	}
	pathEncoding, err := normalizeMapPathEncoding(payload.PathEncoding)
	if err != nil {
		return nil, wrapMapImportFailure(mapImportStagePreflight, "", err)
	}
	stagingRelative := filepath.ToSlash(filepath.Join("staging", job.ID))
	stagingRoot, err := ResolveWithin(e.maps.defaultRoot().Path, filepath.FromSlash(stagingRelative))
	if err != nil {
		return nil, wrapMapImportFailure(mapImportStageStaging, "", err)
	}
	if err := os.Mkdir(stagingRoot, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, wrapMapImportFailure(mapImportStageStaging, "", err)
		}
		if err := removeMapOwnedTree(stagingRoot); err != nil {
			return nil, wrapMapImportFailure(mapImportStageStaging, "", err)
		}
		if err := os.Mkdir(stagingRoot, 0o700); err != nil {
			return nil, wrapMapImportFailure(mapImportStageStaging, "", err)
		}
	}
	defer func() {
		if cleanupErr := removeMapOwnedTree(stagingRoot); cleanupErr != nil && e.logger != nil {
			e.logger.Printf("clean map staging %s: %v", job.ID, cleanupErr)
		}
	}()

	client := newQQFlashClient(e.safeDownloadLimiter())
	progress(3, "enumerating QQ flash share")
	files, deviceID, taskName, shareEntries, err := client.listAllFiles(ctx, payload.Source, maxMapQQFlashFiles, maxMapQQFlashDownload)
	if err != nil {
		return nil, wrapMapImportFailure(mapImportStageMetadata, "", err)
	}
	if len(files) == 0 {
		return nil, wrapMapImportFailure(mapImportStageMetadata, "", errors.New("QQ flash share contains no files"))
	}
	discovery, err := discoverMapInputs(ctx, stagingRoot, files, shareEntries, deviceID, client, pathEncoding, progress)
	if err != nil {
		return nil, wrapMapImportFailure(mapImportStagePreflight, "", err)
	}
	sourceName := files[0].Relative
	if len(discovery.Inputs) > 0 {
		sourceName = strings.SplitN(discovery.Inputs[0].SortKey, "!/", 2)[0]
	}
	progress(38, "validating and merging VPK content")
	result, err := buildMapVPKsWithEncoding(ctx, stagingRoot, discovery.Inputs, discovery.Expanded, taskName, sourceName, pathEncoding)
	if err != nil {
		return nil, wrapMapImportFailure(mapImportStageVPK, sourceName, err)
	}
	if existing, ok := e.maps.findByAggregate(result.AggregateSHA256); ok {
		progress(88, "map content already exists; reusing existing map")
		// Keep the canonical local map ID in the terminal result. Control checks
		// that it belongs to the same node and digest before accepting it.
		progress(100, "map import reused existing map")
		return e.maps.importResult(existing), nil
	}
	result.Warnings = append(mapPathDecodeWarnings(discovery.PathDecodes), result.Warnings...)
	if discovery.Ignored > 0 {
		result.Warnings = append(result.Warnings, mapLocalWarning{Code: "ignored_non_vpk_files", Message: fmt.Sprintf("已跳过分享和嵌套归档中的 %d 个非 VPK 文件", discovery.Ignored)})
	}
	record := mapLocalRecord{ID: payload.MapID, Name: result.Name, Author: result.Author, Version: result.Version,
		Description: result.Description, AggregateSHA256: result.AggregateSHA256,
		SourceName: filepath.Base(filepath.FromSlash(sourceName)), TotalSize: result.TotalSize,
		RemovedFiles: result.RemovedFiles, RemovedBytes: result.RemovedBytes, Artifacts: result.Artifacts,
		BSPs: result.BSPs, Modes: result.Modes, Poster: result.Poster, Warnings: result.Warnings,
		Assignments: make(map[string]mapLocalAssignment)}
	progress(88, "publishing map objects")
	record, err = e.maps.publish(record, result.Outputs, result.PosterOutput)
	if err != nil {
		return nil, wrapMapImportFailure(mapImportStagePublish, sourceName, err)
	}
	progress(100, "map import completed")
	return e.maps.importResult(record), nil
}

const mapHotReloadRCONCommand = "sm_update_vpk"

func (e *Executor) assignMap(ctx context.Context, job domain.Job, mapID string, payload operationPayload, progress ProgressFunc) (map[string]any, error) {
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered on this Agent")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	progress(35, "publishing map to the instance filesystem")
	active := e.processes.HasActiveProcess(job.InstanceID) || record.ExternalPID > 0
	if err := e.maps.assign(mapID, record, instancePath); err != nil {
		return nil, err
	}
	result := mapAssignmentResult(mapID, job.InstanceID)
	if !active {
		progress(100, "map assigned; it will load when the instance starts")
		return result, nil
	}
	if payload.RCONPassword == "" {
		result["hot_reload_skipped"] = "rcon_not_configured"
		progress(100, "map assigned; in-game !update_vpk is required")
		return result, nil
	}
	progress(75, "refreshing the running server VPK list")
	executeRCON := e.mapReloadRCON
	if executeRCON == nil {
		executeRCON = executeLocalRCON
	}
	if _, err := executeRCON(ctx, record, payload, payload.RCONPassword, mapHotReloadRCONCommand, 8*time.Second); err != nil {
		return nil, fmt.Errorf("refresh running server VPK list: %w", err)
	}
	result["hot_reloaded"] = true
	progress(100, "map assigned and running server VPK list refreshed")
	return result, nil
}

func (e *Executor) unassignMap(job domain.Job, mapID string, progress ProgressFunc) (map[string]any, error) {
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered on this Agent")
	}
	if record.ExternalPID > 0 || e.processes.HasActiveProcess(job.InstanceID) || e.processes.State(job.InstanceID).Status != "stopped" {
		return nil, errors.New("instance must be stopped before removing a map")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	progress(35, "removing verified map links")
	if err := e.maps.unassign(mapID, record, instancePath); err != nil {
		return nil, err
	}
	progress(100, "map assignment removed")
	return mapAssignmentResult(mapID, job.InstanceID), nil
}
