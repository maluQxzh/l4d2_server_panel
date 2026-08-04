package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type workshopDownload struct {
	itemID string
	files  []string
}

func (e *Executor) installWorkshop(ctx context.Context, job domain.Job, source string, progress ProgressFunc) (map[string]any, error) {
	if err := validateWorkshopID(source); err != nil {
		return nil, err
	}
	resolver := e.workshopCollectionResolver
	if resolver == nil {
		resolver = ResolveWorkshopCollection
	}
	isCollection, itemIDs, lookupErr := resolver(ctx, source)
	if lookupErr != nil && isCollection {
		return nil, lookupErr
	}
	if !isCollection {
		itemIDs = []string{source}
		if lookupErr != nil && e.logger != nil {
			e.logger.Printf("Workshop collection lookup for %s failed; trying it as an item: %v", source, lookupErr)
		}
	}
	if len(itemIDs) == 0 || len(itemIDs) > maxWorkshopCollectionItems {
		return nil, errors.New("Workshop collection item count is invalid")
	}

	downloads := make([]workshopDownload, 0, len(itemIDs))
	downloadErrors := make([]string, 0)
	for index, itemID := range itemIDs {
		baseProgress := 5 + index*55/len(itemIDs)
		progress(baseProgress, fmt.Sprintf("downloading Workshop item %s (%d/%d)", itemID, index+1, len(itemIDs)))
		files, err := e.steamClient().WorkshopItem(ctx, itemID, func(p int, message string) {
			if p > 0 {
				progress(baseProgress+p*55/(100*len(itemIDs)), message)
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			downloadErrors = append(downloadErrors, fmt.Sprintf("%s: %s", itemID, boundedError(err)))
			continue
		}
		downloads = append(downloads, workshopDownload{itemID: itemID, files: files})
	}
	if len(downloadErrors) > 0 {
		return nil, fmt.Errorf("Workshop download failed for %d item(s): %s", len(downloadErrors), strings.Join(downloadErrors, "; "))
	}

	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	type pendingFile struct {
		itemID   string
		source   string
		relative string
		version  int
		hash     string
	}
	pending := make([]pendingFile, 0)
	destinations := make(map[string]bool)
	for _, download := range downloads {
		for _, file := range download.files {
			version, hash, err := validateVPK(file)
			if err != nil {
				return nil, fmt.Errorf("Workshop item %s contains an invalid VPK: %w", download.itemID, err)
			}
			name := workshopInstalledVPKName(download.itemID, filepath.Base(file))
			relative := filepath.ToSlash(filepath.Join("left4dead2", "addons", name))
			key := strings.ToLower(filepath.Clean(relative))
			if destinations[key] {
				return nil, fmt.Errorf("Workshop items produce the same addon path: %s", relative)
			}
			destinations[key] = true
			pending = append(pending, pendingFile{itemID: download.itemID, source: file, relative: relative, version: version, hash: hash})
		}
	}
	if len(pending) == 0 {
		return nil, errors.New("Workshop download contained no installable VPK files")
	}
	conflictCandidates := make([]vpkInstallCandidate, 0, len(pending))
	for _, file := range pending {
		conflictCandidates = append(conflictCandidates, vpkInstallCandidate{
			Source: file.source, TargetRelative: file.relative,
		})
	}
	if err := validateVPKContentConflicts(instancePath, conflictCandidates); err != nil {
		return nil, fmt.Errorf("Workshop VPK content conflict: %w", err)
	}

	progress(65, "installing validated Workshop VPK files")
	created := make([]string, 0, len(pending))
	installed := make([]map[string]any, 0, len(pending))
	for _, file := range pending {
		if err := copyRegularFileToRootNoFollow(file.source, instancePath, file.relative, secureWriteOptions{
			Mode: 0o640, ExpectedSHA256: file.hash, Replace: false,
		}); err != nil {
			for index := len(created) - 1; index >= 0; index-- {
				_ = deleteManagedFileNoFollow(instancePath, created[index])
			}
			return nil, fmt.Errorf("install Workshop item %s: %w", file.itemID, err)
		}
		created = append(created, file.relative)
		installed = append(installed, map[string]any{
			"source_id": file.itemID, "path": file.relative, "sha256": file.hash, "vpk_version": file.version,
		})
	}
	progress(100, "Workshop package installed")
	result := map[string]any{
		"kind": "workshop", "source_id": source, "source_ids": itemIDs, "collection": isCollection, "files": installed,
	}
	if lookupErr != nil {
		result["collection_lookup_warning"] = boundedError(lookupErr)
	}
	return result, nil
}

func workshopInstalledVPKName(itemID, sourceName string) string {
	if strings.HasSuffix(strings.ToLower(sourceName), "_legacy.bin") {
		sourceName = sourceName[:len(sourceName)-len("_legacy.bin")] + ".vpk"
	}
	return itemID + "_" + sourceName
}

func boundedError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		return message[:300] + "..."
	}
	return message
}
