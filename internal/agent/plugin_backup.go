package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	pluginBackupLayoutPreserve = "preserve"
	pluginBackupLayoutCompact  = "compact"
	maxPluginBackupPackages    = 1000
)

type pluginBackupPayload struct {
	Layout   string                       `json:"layout"`
	Packages []domain.PluginBackupPackage `json:"plugin_packages"`
}

func (e *Executor) pluginBackup(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	rawPayload, err := json.Marshal(job.Payload)
	if err != nil {
		return nil, err
	}
	var payload pluginBackupPayload
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		return nil, err
	}
	layout := strings.ToLower(strings.TrimSpace(payload.Layout))
	if layout != pluginBackupLayoutPreserve && layout != pluginBackupLayoutCompact {
		return nil, errors.New("plugin backup layout must be preserve or compact")
	}
	if len(payload.Packages) == 0 || len(payload.Packages) > maxPluginBackupPackages {
		return nil, errors.New("plugin backup package count is invalid")
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	folders, fileCount, err := pluginBackupFolders(payload.Packages)
	if err != nil {
		return nil, err
	}
	backupsRoot, err := ensureDataSubdirectory(e.config.DataDir, "backups")
	if err != nil {
		return nil, err
	}
	instanceBackupDir := filepath.Join(backupsRoot, safeLogName(job.InstanceID))
	if err := os.MkdirAll(instanceBackupDir, 0o750); err != nil {
		return nil, err
	}
	instanceBackupDir, err = resolveBelow(backupsRoot, instanceBackupDir)
	if err != nil {
		return nil, err
	}
	createdAt := time.Now().UTC()
	name := "plugin-" + createdAt.Format("20060102T150405.000000000Z") + "-" + safeLogName(job.ID) + ".7z"
	destination, err := ResolveWithin(instanceBackupDir, name)
	if err != nil {
		return nil, err
	}
	temporary := destination + ".tmp"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	committed := false
	published := false
	defer func() {
		_ = output.Close()
		if !committed {
			_ = os.Remove(temporary)
			if published {
				_ = os.Remove(destination)
			}
		}
	}()
	archive, err := newSevenZipWriter(output)
	if err != nil {
		return nil, err
	}
	progress(10, "validating plugin inventory")
	totalSize := int64(0)
	writtenFiles := 0
	entryPaths := make(map[string]bool, fileCount)
	for packageIndex, item := range payload.Packages {
		for _, managed := range item.Files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			sourceRoot, sourceRelative, err := e.pluginBackupSource(job.InstanceID, instancePath, item, managed)
			if err != nil {
				return nil, fmt.Errorf("prepare plugin %q file %q: %w", item.Name, managed.Path, err)
			}
			source, err := openManagedFileNoFollow(sourceRoot, sourceRelative)
			if err != nil {
				return nil, fmt.Errorf("open plugin %q file %q: %w", item.Name, managed.Path, err)
			}
			info, statErr := source.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSmartPluginEntry {
				_ = source.Close()
				return nil, fmt.Errorf("plugin %q file %q is not a bounded regular file", item.Name, managed.Path)
			}
			if managed.Size > 0 && managed.Size != info.Size() {
				_ = source.Close()
				return nil, fmt.Errorf("plugin %q file %q size changed", item.Name, managed.Path)
			}
			entryPath := pluginBackupEntryPath(folders[packageIndex], managed.Path, layout)
			entryKey := strings.ToLower(entryPath)
			if entryPaths[entryKey] {
				_ = source.Close()
				return nil, fmt.Errorf("plugin backup entry %q is duplicated", entryPath)
			}
			entryPaths[entryKey] = true
			size, _, addErr := archive.Add(entryPath, io.LimitReader(&contextReader{ctx: ctx, reader: source}, maxSmartPluginEntry+1))
			closeErr := source.Close()
			if addErr != nil {
				return nil, addErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if size > uint64(maxSmartPluginEntry) || int64(size) != info.Size() {
				return nil, fmt.Errorf("plugin %q file %q changed while reading", item.Name, managed.Path)
			}
			totalSize += int64(size)
			if totalSize > maxSmartPluginExpanded {
				return nil, errors.New("plugin backup exceeds the expanded size limit")
			}
			writtenFiles++
			progress(10+writtenFiles*80/fileCount, "compressing plugin files")
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := output.Sync(); err != nil {
		return nil, err
	}
	if err := output.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return nil, err
	}
	published = true
	archiveSHA, archiveSize, err := hashFile(destination)
	if err != nil {
		return nil, err
	}
	committed = true
	progress(100, "plugin backup created")
	return map[string]any{
		"path": destination, "name": name, "kind": "plugin_" + layout,
		"size": archiveSize, "sha256": archiveSHA, "platform": runtime.GOOS + "/" + runtime.GOARCH,
		"files": writtenFiles, "package_count": len(payload.Packages), "created_at": createdAt,
	}, nil
}

func pluginBackupFolders(packages []domain.PluginBackupPackage) ([]string, int, error) {
	folders := make([]string, len(packages))
	counts := make(map[string]int, len(packages))
	fileCount := 0
	for index, item := range packages {
		kind := strings.ToLower(strings.TrimSpace(item.Kind))
		if item.ID == "" || len(item.ID) > 200 || item.Name == "" || len(item.Name) > 500 ||
			(kind != "smart" && kind != "plugin" && kind != "smx" && kind != "native") || len(item.Files) == 0 {
			return nil, 0, errors.New("plugin backup package metadata is invalid")
		}
		folder, err := sanitizeWorkshopTitle(item.Name)
		if err == nil {
			err = validateQQFlashEntryName(folder)
		}
		if err != nil {
			folder = "plugin-" + pluginBackupID(item.ID)
		}
		folders[index] = folder
		counts[strings.ToLower(folder)]++
		fileCount += len(item.Files)
		if fileCount > maxSmartPluginFiles {
			return nil, 0, errors.New("plugin backup file count exceeds the limit")
		}
	}
	used := make(map[string]bool, len(folders))
	for index, folder := range folders {
		if counts[strings.ToLower(folder)] > 1 {
			folder = truncateUTF8(folder, 128) + "-" + pluginBackupID(packages[index].ID)
		}
		key := strings.ToLower(folder)
		if used[key] {
			return nil, 0, errors.New("plugin backup folder names are not unique")
		}
		used[key], folders[index] = true, folder
	}
	return folders, fileCount, nil
}

func (e *Executor) pluginBackupSource(instanceID, instancePath string, item domain.PluginBackupPackage, managed domain.PluginBackupFile) (string, string, error) {
	path := filepath.ToSlash(strings.TrimSpace(managed.Path))
	if managed.Size < 0 || len(managed.SHA256) != sha256.Size*2 {
		return "", "", errors.New("plugin backup file metadata is invalid")
	}
	if _, err := hex.DecodeString(managed.SHA256); err != nil {
		return "", "", errors.New("plugin backup file SHA-256 is invalid")
	}
	extension := strings.ToLower(filepath.Ext(path))
	if !smartPluginExtensions[extension] {
		return "", "", errors.New("plugin backup file type is not approved")
	}
	if err := validateSmartPluginTarget(path, extension); err != nil {
		return "", "", err
	}
	if !strings.EqualFold(item.Kind, "smart") || !managed.Entrypoint {
		return instancePath, path, nil
	}
	state, stateRoot, err := e.readSmartPluginState(instanceID, item.ID)
	if err != nil {
		return "", "", err
	}
	for _, file := range state.Files {
		if file.TargetPath != path {
			continue
		}
		if !file.Entrypoint || !strings.EqualFold(file.SHA256, managed.SHA256) {
			return "", "", errors.New("smart plugin entrypoint does not match its state")
		}
		if state.Enabled {
			return instancePath, path, nil
		}
		return stateRoot, filepath.ToSlash(filepath.Join("disabled", file.ID+filepath.Ext(file.TargetPath))), nil
	}
	return "", "", errors.New("smart plugin entrypoint is missing from its state")
}

func pluginBackupEntryPath(folder, managedPath, layout string) string {
	path := filepath.ToSlash(managedPath)
	if layout == pluginBackupLayoutCompact {
		lower := strings.ToLower(path)
		const sourcemodRoot = "left4dead2/addons/sourcemod/"
		if strings.HasPrefix(lower, sourcemodRoot) {
			path = path[len(sourcemodRoot):]
		} else if strings.HasPrefix(lower, "left4dead2/") {
			path = path[len("left4dead2/"):]
		}
	}
	return folder + "/" + path
}

func pluginBackupID(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:4])
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
