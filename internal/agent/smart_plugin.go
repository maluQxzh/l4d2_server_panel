package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

const (
	smartPluginPlanVersion = 1
	maxSmartPluginFolders  = 256
	maxSmartPluginFiles    = 5000
	maxSmartPluginEntry    = int64(512 << 20)
	maxSmartPluginExpanded = int64(2 << 30)
)

var smartPluginExtensions = map[string]bool{
	".smx": true, ".sp": true, ".inc": true, ".txt": true, ".cfg": true,
	".nut": true, ".dll": true, ".so": true, ".vdf": true, ".autoload": true,
}

var editableSmartPluginExtensions = map[string]bool{
	".cfg": true, ".txt": true, ".sp": true, ".inc": true, ".nut": true,
}

type smartPluginFile struct {
	ID               string                          `json:"id"`
	SourcePath       string                          `json:"source_path"`
	TargetPath       string                          `json:"target_path,omitempty"`
	SuggestedPath    string                          `json:"suggested_path,omitempty"`
	Choices          []string                        `json:"choices,omitempty"`
	Reason           string                          `json:"reason"`
	Ambiguous        bool                            `json:"ambiguous"`
	Native           bool                            `json:"native"`
	Entrypoint       bool                            `json:"entrypoint"`
	Existing         bool                            `json:"existing"`
	ExistingSHA256   string                          `json:"existing_sha256,omitempty"`
	ManagedPackageID string                          `json:"managed_package_id,omitempty"`
	PathStates       map[string]smartPluginPathState `json:"path_states,omitempty"`
	Size             int64                           `json:"size"`
	SHA256           string                          `json:"sha256"`
}

type smartPluginPathState struct {
	Existing         bool   `json:"existing"`
	ExistingSHA256   string `json:"existing_sha256,omitempty"`
	ManagedPackageID string `json:"managed_package_id,omitempty"`
}

type smartPluginFolder struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	SourcePrefix string            `json:"source_prefix,omitempty"`
	Files        []smartPluginFile `json:"files"`
	NativeRisk   bool              `json:"native_risk"`
	Ambiguous    int               `json:"ambiguous_files"`
	Conflicts    int               `json:"existing_files"`
}

type smartPluginPlan struct {
	Version       int                 `json:"version"`
	ArchiveName   string              `json:"archive_name"`
	ArchiveSHA256 string              `json:"archive_sha256"`
	Fingerprint   string              `json:"fingerprint"`
	Folders       []smartPluginFolder `json:"folders"`
	FileCount     int                 `json:"file_count"`
	ExpandedBytes int64               `json:"expanded_bytes"`
	Format        string              `json:"format"`
}

type smartPluginSelection struct {
	FolderID  string            `json:"folder_id"`
	PackageID string            `json:"package_id,omitempty"`
	Overrides map[string]string `json:"overrides,omitempty"`
	Takeovers []string          `json:"takeovers,omitempty"`
}

type smartPluginStateFile struct {
	ID             string `json:"id"`
	TargetPath     string `json:"target_path"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	Entrypoint     bool   `json:"entrypoint"`
	Baseline       bool   `json:"baseline"`
	BaselineSHA256 string `json:"baseline_sha256,omitempty"`
}

type smartPluginState struct {
	Version      int                    `json:"version"`
	PackageID    string                 `json:"package_id"`
	InstanceID   string                 `json:"instance_id"`
	Name         string                 `json:"name"`
	Enabled      bool                   `json:"enabled"`
	ArchiveSHA   string                 `json:"archive_sha256"`
	AggregateSHA string                 `json:"aggregate_sha256"`
	Files        []smartPluginStateFile `json:"files"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

type smartRollbackFile struct {
	target, backup string
	existed        bool
}

func (e *Executor) inspectSmartPluginArchive(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	archivePath, archiveSHA, cleanup, err := e.stageSmartPluginUpload(job.ID, payload.Name, payload.ContentBase64)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	progress(20, "extracting plugin archive safely")
	plan, _, extractedCleanup, err := e.prepareSmartPluginPlan(ctx, archivePath, payload.Name, archiveSHA, instancePath)
	if extractedCleanup != nil {
		defer extractedCleanup()
	}
	if err != nil {
		return nil, err
	}
	if err := overlaySmartManagedPaths(&plan, payload.RegisteredPaths); err != nil {
		return nil, err
	}
	progress(100, "plugin archive inspection completed")
	return structToMap(plan)
}

func (e *Executor) stageSmartPluginUpload(jobID, name, encoded string) (string, string, func(), error) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "\x00/\\") {
		return "", "", nil, errors.New("plugin archive name is invalid")
	}
	if encoded == "" || encoded == "[encrypted]" || len(encoded) > protocol.MaxEncodedPackageUploadBytes {
		return "", "", nil, errors.New("plugin archive content is required and must not exceed 100 MiB")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > protocol.MaxPackageUploadBytes {
		return "", "", nil, errors.New("plugin archive is not valid Base64 or exceeds 100 MiB")
	}
	staging, err := ensureDataSubdirectory(e.config.DataDir, "staging")
	if err != nil {
		return "", "", nil, err
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".gz" && strings.HasSuffix(strings.ToLower(name), ".tar.gz") {
		ext = ".tar.gz"
	}
	path, err := ResolveWithin(staging, "smart-upload-"+safeLogName(jobID)+ext)
	if err != nil {
		return "", "", nil, err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", "", nil, err
	}
	digest := sha256.Sum256(raw)
	return path, hex.EncodeToString(digest[:]), func() { _ = os.Remove(path) }, nil
}

func (e *Executor) prepareSmartPluginPlan(ctx context.Context, archivePath, archiveName, archiveSHA, instancePath string) (smartPluginPlan, string, func(), error) {
	kind, err := detectMapFileKind(archivePath)
	if err != nil {
		return smartPluginPlan{}, "", nil, err
	}
	format := ""
	switch kind {
	case mapFileZIP:
		format = "zip"
	case mapFile7Z:
		format = "7z"
	case mapFileRAR:
		format = "rar"
	default:
		return smartPluginPlan{}, "", nil, errors.New("smart plugin archives must be ZIP, 7Z, or single-volume RAR")
	}
	staging, err := ensureDataSubdirectory(e.config.DataDir, "staging")
	if err != nil {
		return smartPluginPlan{}, "", nil, err
	}
	extracted, err := os.MkdirTemp(staging, "smart-extracted-*")
	if err != nil {
		return smartPluginPlan{}, "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(extracted) }
	state := &mapArchiveState{
		Root: extracted, MaxEntries: maxSmartPluginFiles, MaxEntrySize: maxSmartPluginEntry,
		MaxExpanded: maxSmartPluginExpanded, DiskFreeReserve: 256 << 20, Subject: "plugin",
	}
	files, err := extractMapArchive(ctx, archivePath, extracted, kind, state)
	if err != nil {
		cleanup()
		return smartPluginPlan{}, "", nil, err
	}
	if len(files) == 0 {
		cleanup()
		return smartPluginPlan{}, "", nil, errors.New("plugin archive contains no regular files")
	}
	plan, err := buildSmartPluginPlan(extracted, archiveName, archiveSHA, format, state.Expanded, files, instancePath)
	if err != nil {
		cleanup()
		return smartPluginPlan{}, "", nil, err
	}
	return plan, extracted, cleanup, nil
}

func buildSmartPluginPlan(extracted, archiveName, archiveSHA, format string, expanded int64, files []extractedMapFile, instancePath string) (smartPluginPlan, error) {
	groups, err := groupSmartPluginFiles(archiveName, files)
	if err != nil {
		return smartPluginPlan{}, err
	}
	if len(groups) > maxSmartPluginFolders {
		return smartPluginPlan{}, errors.New("plugin archive contains more than 256 plugin folders")
	}
	plan := smartPluginPlan{Version: smartPluginPlanVersion, ArchiveName: archiveName, ArchiveSHA256: archiveSHA, Format: format, ExpandedBytes: expanded}
	seenTargets := make(map[string]string)
	for _, group := range groups {
		folder := smartPluginFolder{ID: smartStableID("folder", group.name), Name: group.name, SourcePrefix: group.prefix}
		siblingNames := make(map[string]bool)
		for _, item := range group.files {
			siblingNames[strings.ToLower(filepath.Base(item.Relative))] = true
		}
		for _, item := range group.files {
			relative := strings.TrimPrefix(item.Relative, group.prefix)
			relative = strings.TrimPrefix(relative, "/")
			mapped, err := mapSmartPluginFile(relative, siblingNames)
			if err != nil {
				return smartPluginPlan{}, fmt.Errorf("plugin %q: %w", group.name, err)
			}
			mapped.SourcePath = item.Relative
			mapped.ID = smartStableID("file", item.Relative)
			hash, size, err := hashFile(item.Path)
			if err != nil {
				return smartPluginPlan{}, err
			}
			mapped.SHA256, mapped.Size = hash, size
			candidate := firstNonEmpty(mapped.TargetPath, mapped.SuggestedPath)
			if candidate != "" {
				key := strings.ToLower(candidate)
				if previous := seenTargets[key]; previous != "" {
					return smartPluginPlan{}, fmt.Errorf("plugin files %q and %q map to the same target %q", previous, item.Relative, candidate)
				}
				seenTargets[key] = item.Relative
			}
			mapped.PathStates = make(map[string]smartPluginPathState)
			inspectPaths := append([]string{candidate}, mapped.Choices...)
			for _, inspectPath := range inspectPaths {
				if inspectPath == "" {
					continue
				}
				if _, duplicate := mapped.PathStates[inspectPath]; duplicate {
					continue
				}
				pathState := smartPluginPathState{}
				if existingSHA, _, hashErr := hashRegularFileWithinNoFollow(instancePath, inspectPath, maxSmartPluginEntry); hashErr == nil {
					pathState.Existing, pathState.ExistingSHA256 = true, existingSHA
				} else if !errors.Is(hashErr, os.ErrNotExist) {
					return smartPluginPlan{}, fmt.Errorf("inspect existing target %q: %w", inspectPath, hashErr)
				}
				mapped.PathStates[inspectPath] = pathState
			}
			primaryState := mapped.PathStates[candidate]
			mapped.Existing, mapped.ExistingSHA256 = primaryState.Existing, primaryState.ExistingSHA256
			if mapped.Native {
				folder.NativeRisk = true
			}
			if mapped.Ambiguous {
				folder.Ambiguous++
			}
			if mapped.Existing {
				folder.Conflicts++
			}
			folder.Files = append(folder.Files, mapped)
			plan.FileCount++
		}
		sort.Slice(folder.Files, func(i, j int) bool { return folder.Files[i].SourcePath < folder.Files[j].SourcePath })
		plan.Folders = append(plan.Folders, folder)
	}
	sort.Slice(plan.Folders, func(i, j int) bool { return plan.Folders[i].Name < plan.Folders[j].Name })
	plan.Fingerprint, err = smartPlanFingerprint(plan)
	return plan, err
}

type smartFileGroup struct {
	name   string
	prefix string
	files  []extractedMapFile
}

func groupSmartPluginFiles(archiveName string, files []extractedMapFile) ([]smartFileGroup, error) {
	hasRootFile := false
	top := make(map[string][]extractedMapFile)
	for _, file := range files {
		parts := strings.Split(file.Relative, "/")
		if len(parts) == 1 {
			hasRootFile = true
		}
		top[parts[0]] = append(top[parts[0]], file)
	}
	archiveBase := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(archiveName, filepath.Ext(archiveName)), ".tar"), ".plugin"), ".archive")
	if hasRootFile {
		return []smartFileGroup{{name: archiveBase, files: files}}, nil
	}
	if len(top) == 1 {
		for outer, outerFiles := range top {
			children := make(map[string][]extractedMapFile)
			outerIsPlugin := false
			for _, file := range outerFiles {
				rest := strings.TrimPrefix(file.Relative, outer+"/")
				parts := strings.Split(rest, "/")
				if smartExplicitRoot(parts[0]) || len(parts) == 1 {
					outerIsPlugin = true
				}
				children[parts[0]] = append(children[parts[0]], file)
			}
			if !outerIsPlugin {
				groups := make([]smartFileGroup, 0, len(children))
				for child, childFiles := range children {
					groups = append(groups, smartFileGroup{name: child, prefix: outer + "/" + child, files: childFiles})
				}
				sort.Slice(groups, func(i, j int) bool { return groups[i].name < groups[j].name })
				return groups, nil
			}
			return []smartFileGroup{{name: outer, prefix: outer, files: outerFiles}}, nil
		}
	}
	groups := make([]smartFileGroup, 0, len(top))
	for name, grouped := range top {
		groups = append(groups, smartFileGroup{name: name, prefix: name, files: grouped})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].name < groups[j].name })
	return groups, nil
}

func smartExplicitRoot(value string) bool {
	switch strings.ToLower(value) {
	case "left4dead2", "addons", "cfg", "modes", "scripts", "plugins", "scripting", "gamedata", "translations", "configs", "extensions", "data":
		return true
	default:
		return false
	}
}

func mapSmartPluginFile(relative string, siblings map[string]bool) (smartPluginFile, error) {
	relative = filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if relative == "." || strings.HasPrefix(relative, "../") || strings.HasPrefix(relative, "/") {
		return smartPluginFile{}, ErrPathEscape
	}
	ext := strings.ToLower(filepath.Ext(relative))
	if !smartPluginExtensions[ext] {
		return smartPluginFile{}, fmt.Errorf("unsupported smart plugin file %q", relative)
	}
	file := smartPluginFile{Native: ext == ".dll" || ext == ".so", Entrypoint: ext == ".smx" || ext == ".dll" || ext == ".so" || ext == ".vdf" || ext == ".autoload"}
	lower := strings.ToLower(relative)
	if strings.HasPrefix(lower, "left4dead2/") {
		file.TargetPath, file.Reason = relative, "压缩包已提供完整游戏路径"
	} else if strings.HasPrefix(lower, "addons/") || strings.HasPrefix(lower, "cfg/") || strings.HasPrefix(lower, "modes/") || strings.HasPrefix(lower, "scripts/vscripts/") {
		file.TargetPath, file.Reason = "left4dead2/"+relative, "压缩包已提供游戏内相对路径"
	} else {
		first := strings.ToLower(strings.Split(relative, "/")[0])
		if map[string]bool{"plugins": true, "scripting": true, "gamedata": true, "translations": true, "configs": true, "extensions": true, "data": true}[first] {
			file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/"+relative, "识别为 SourceMod 相对目录"
		} else if strings.Contains(relative, "/") {
			return smartPluginFile{}, fmt.Errorf("file %q is beneath an unrecognized plugin directory", relative)
		}
	}
	if file.TargetPath == "" {
		base := filepath.Base(relative)
		switch ext {
		case ".smx":
			file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/plugins/"+base, "SMX 插件固定目录"
		case ".sp":
			file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/scripting/"+base, "SourcePawn 源码固定目录"
		case ".inc":
			file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/scripting/include/"+base, "SourcePawn include 固定目录"
		case ".nut":
			file.TargetPath, file.Reason = "left4dead2/scripts/vscripts/"+base, "VScript 固定目录"
		case ".autoload":
			file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/extensions/"+base, "SourceMod 扩展加载标记"
		case ".vdf":
			file.TargetPath, file.Reason = "left4dead2/addons/"+base, "Metamod 插件描述文件"
		case ".txt":
			lowerBase := strings.ToLower(base)
			switch {
			case strings.HasSuffix(lowerBase, ".phrases.txt"):
				file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/translations/"+base, "phrases 文件名"
			case strings.HasSuffix(lowerBase, ".games.txt"):
				file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/gamedata/"+base, "games 文件名"
			case smartModeTextName(lowerBase) && siblings[strings.TrimSuffix(lowerBase, ".txt")+".nut"]:
				file.TargetPath, file.Reason = "left4dead2/modes/"+base, "模式文件名并存在同名 VScript"
			default:
				file.Ambiguous, file.SuggestedPath = true, "left4dead2/addons/sourcemod/gamedata/"+base
				file.Choices = []string{file.SuggestedPath, "left4dead2/addons/sourcemod/translations/" + base, "left4dead2/addons/sourcemod/configs/" + base, "left4dead2/modes/" + base}
				file.Reason = "TXT 可能是 gamedata、翻译、配置或模式文件"
			}
		case ".cfg":
			file.Ambiguous, file.SuggestedPath = true, "left4dead2/cfg/sourcemod/"+base
			file.Choices = []string{file.SuggestedPath, "left4dead2/addons/sourcemod/configs/" + base, "left4dead2/addons/sourcemod/data/" + base}
			file.Reason = "CFG 可能是运行配置、SourceMod configs 或 data"
		case ".dll", ".so":
			lowerBase := strings.ToLower(base)
			if strings.Contains(lowerBase, ".ext.") || strings.Contains(lowerBase, ".ext") {
				file.TargetPath, file.Reason = "left4dead2/addons/sourcemod/extensions/"+base, "扩展文件名包含 .ext"
			} else if siblings[strings.TrimSuffix(lowerBase, ext)+".vdf"] {
				file.TargetPath, file.Reason = "left4dead2/addons/"+base, "存在同名 Metamod VDF"
			} else {
				file.Ambiguous, file.SuggestedPath = true, "left4dead2/addons/sourcemod/extensions/"+base
				file.Choices = []string{file.SuggestedPath, "left4dead2/addons/" + base}
				file.Reason = "裸原生库无法区分 SourceMod 扩展与 Metamod 插件"
			}
		}
	}
	if candidate := firstNonEmpty(file.TargetPath, file.SuggestedPath); candidate != "" {
		if err := validateSmartPluginTarget(candidate, ext); err != nil {
			return smartPluginFile{}, err
		}
	}
	return file, nil
}

func smartModeTextName(base string) bool {
	if !strings.HasSuffix(base, ".txt") {
		return false
	}
	stem := strings.TrimSuffix(base, ".txt")
	if len(stem) < 2 || !strings.ContainsRune("cmr", rune(stem[0])) {
		return false
	}
	return strings.Trim(stem[1:], "0123456789") == ""
}

func validateSmartPluginTarget(target, sourceExt string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(target))))
	if cleaned != target || filepath.IsAbs(filepath.FromSlash(cleaned)) || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, `\`) {
		return errors.New("smart plugin target path is invalid")
	}
	lower := strings.ToLower(cleaned)
	allowed := strings.HasPrefix(lower, "left4dead2/addons/") || strings.HasPrefix(lower, "left4dead2/cfg/") || strings.HasPrefix(lower, "left4dead2/modes/") || strings.HasPrefix(lower, "left4dead2/scripts/vscripts/")
	if !allowed || strings.ToLower(filepath.Ext(cleaned)) != sourceExt {
		return fmt.Errorf("smart plugin target %q is outside the approved directories or changes the file type", target)
	}
	for _, component := range strings.Split(cleaned, "/") {
		if err := validateQQFlashEntryName(component); err != nil {
			return errors.New("smart plugin target contains an unsafe path component")
		}
	}
	return nil
}

func smartStableID(kind, value string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + strings.ToLower(filepath.ToSlash(value))))
	return hex.EncodeToString(digest[:16])
}

func smartPlanFingerprint(plan smartPluginPlan) (string, error) {
	copyPlan := plan
	copyPlan.Fingerprint = ""
	raw, err := json.Marshal(copyPlan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func overlaySmartManagedPaths(plan *smartPluginPlan, owners map[string]string) error {
	for folderIndex := range plan.Folders {
		for fileIndex := range plan.Folders[folderIndex].Files {
			file := &plan.Folders[folderIndex].Files[fileIndex]
			candidate := firstNonEmpty(file.TargetPath, file.SuggestedPath)
			for target, state := range file.PathStates {
				state.ManagedPackageID = owners[strings.ToLower(filepath.ToSlash(target))]
				file.PathStates[target] = state
			}
			file.ManagedPackageID = file.PathStates[candidate].ManagedPackageID
		}
	}
	var err error
	plan.Fingerprint, err = smartPlanFingerprint(*plan)
	return err
}

func structToMap(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Executor) installSmartPluginArchive(ctx context.Context, job domain.Job, payload operationPayload, archivePath, archiveSHA, instancePath string, progress ProgressFunc) (map[string]any, error) {
	plan, extracted, cleanup, err := e.prepareSmartPluginPlan(ctx, archivePath, payload.Name, archiveSHA, instancePath)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}
	if err := overlaySmartManagedPaths(&plan, payload.RegisteredPaths); err != nil {
		return nil, err
	}
	if payload.InspectionFingerprint == "" || payload.InspectionFingerprint != plan.Fingerprint {
		return nil, errors.New("plugin archive no longer matches the inspected plan")
	}
	if len(payload.SmartPackages) == 0 {
		return nil, errors.New("select at least one inspected plugin folder")
	}
	running := e.smartPluginServerRunning(job.InstanceID)
	if running && (!payload.AcknowledgeLiveChanges || !payload.AcknowledgeRestartRequired) {
		return nil, errors.New("running-instance smart plugin changes require both live-change and manual-restart acknowledgements")
	}
	selected := make(map[string]smartPluginSelection)
	selectedPackageIDs := make(map[string]bool)
	for _, selection := range payload.SmartPackages {
		if selection.FolderID == "" || selected[selection.FolderID].FolderID != "" {
			return nil, errors.New("smart plugin selections contain a missing or duplicate folder id")
		}
		selected[selection.FolderID] = selection
		if selection.PackageID != "" {
			if selectedPackageIDs[selection.PackageID] {
				return nil, errors.New("smart plugin selections contain a duplicate package id")
			}
			selectedPackageIDs[selection.PackageID] = true
		}
	}
	type pendingFile struct {
		folderID, packageID, packageName string
		file                             smartPluginFile
		target, source                   string
		takeover                         bool
		existingSHA                      string
		stateFileID                      string
		baseline                         bool
		baselineSHA                      string
	}
	pending := make([]pendingFile, 0)
	selectedTargets := make(map[string]string)
	packageIDs := make(map[string]string)
	updateStates := make(map[string]smartPluginState)
	updateRoots := make(map[string]string)
	nativeRisk := false
	for _, folder := range plan.Folders {
		selection, ok := selected[folder.ID]
		if !ok {
			continue
		}
		packageID := strings.TrimSpace(selection.PackageID)
		if packageID == "" {
			packageID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(job.ID+":"+folder.ID)).String()
		} else if uuid.Validate(packageID) != nil {
			return nil, errors.New("smart plugin update package id is invalid")
		}
		if selection.PackageID != "" {
			state, stateRoot, stateErr := e.readSmartPluginState(job.InstanceID, packageID)
			if stateErr != nil {
				return nil, fmt.Errorf("load smart plugin update state: %w", stateErr)
			}
			if !state.Enabled {
				return nil, errors.New("enable a smart plugin before updating it")
			}
			updateStates[packageID], updateRoots[packageID] = state, stateRoot
		}
		packageIDs[folder.ID] = packageID
		takeovers := make(map[string]bool, len(selection.Takeovers))
		for _, id := range selection.Takeovers {
			takeovers[id] = true
		}
		for _, file := range folder.Files {
			target := file.TargetPath
			if override := strings.TrimSpace(selection.Overrides[file.ID]); override != "" {
				if err := validateSmartPluginTarget(override, strings.ToLower(filepath.Ext(file.SourcePath))); err != nil {
					return nil, fmt.Errorf("override %q: %w", file.SourcePath, err)
				}
				target = override
			}
			if target == "" {
				return nil, fmt.Errorf("ambiguous file %q needs an approved target", file.SourcePath)
			}
			targetKey := strings.ToLower(filepath.ToSlash(target))
			if previous := selectedTargets[targetKey]; previous != "" {
				return nil, fmt.Errorf("selected files %q and %q use the same target %q", previous, file.SourcePath, target)
			}
			selectedTargets[targetKey] = file.SourcePath
			registeredOwner := payload.RegisteredPaths[targetKey]
			if registeredOwner != "" && registeredOwner != packageID {
				return nil, fmt.Errorf("target %q is already managed by another package", target)
			}
			existingSHA := ""
			if hash, _, hashErr := hashRegularFileWithinNoFollow(instancePath, target, maxSmartPluginEntry); hashErr == nil {
				existingSHA = hash
				if registeredOwner == "" && selection.PackageID == "" && !takeovers[file.ID] {
					return nil, fmt.Errorf("existing target %q requires explicit takeover confirmation", target)
				}
			} else if !errors.Is(hashErr, os.ErrNotExist) {
				return nil, fmt.Errorf("inspect final target %q: %w", target, hashErr)
			}
			source, err := ResolveWithin(extracted, filepath.FromSlash(file.SourcePath))
			if err != nil {
				return nil, err
			}
			item := pendingFile{folderID: folder.ID, packageID: packageID, packageName: folder.Name, file: file, target: target, source: source, takeover: takeovers[file.ID], existingSHA: existingSHA, stateFileID: file.ID}
			if old, updating := updateStates[packageID]; updating {
				matched := false
				for _, oldFile := range old.Files {
					if strings.EqualFold(oldFile.TargetPath, target) {
						if !strings.EqualFold(existingSHA, oldFile.SHA256) {
							return nil, fmt.Errorf("managed update target %q changed or is missing", target)
						}
						item.stateFileID, item.baseline, item.baselineSHA = oldFile.ID, oldFile.Baseline, oldFile.BaselineSHA256
						matched = true
						break
					}
				}
				if !matched {
					return nil, fmt.Errorf("smart plugin updates cannot add or move managed path %q; install it as a new package", target)
				}
			}
			pending = append(pending, item)
			nativeRisk = nativeRisk || file.Native
		}
	}
	if len(pending) == 0 || len(packageIDs) != len(selected) {
		return nil, errors.New("one or more selected plugin folders are not present in the inspected archive")
	}
	for packageID, old := range updateStates {
		count := 0
		for _, item := range pending {
			if item.packageID == packageID {
				count++
			}
		}
		if count != len(old.Files) {
			return nil, errors.New("smart plugin updates must contain the same managed path set as the installed package")
		}
	}
	if nativeRisk && !payload.AcknowledgeRisk && !payload.RiskConfirmed {
		return nil, errors.New("smart plugin archive contains native code and requires acknowledge_native_risk")
	}
	progress(45, "creating atomic plugin rollback state")
	rollbackRoot, err := os.MkdirTemp(filepath.Dir(extracted), "smart-rollback-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(rollbackRoot)
	rollbacks := make([]smartRollbackFile, 0, len(pending))
	for _, item := range pending {
		backup := filepath.ToSlash(filepath.Join("files", item.file.ID+filepath.Ext(item.target)))
		if _, _, hashErr := hashRegularFileWithinNoFollow(instancePath, item.target, maxSmartPluginEntry); hashErr == nil {
			if err := copyManagedFileBetweenRoots(instancePath, item.target, rollbackRoot, backup); err != nil {
				return nil, err
			}
			rollbacks = append(rollbacks, smartRollbackFile{target: item.target, backup: backup, existed: true})
		} else if errors.Is(hashErr, os.ErrNotExist) {
			rollbacks = append(rollbacks, smartRollbackFile{target: item.target})
		} else {
			return nil, hashErr
		}
	}
	rollback := func() error {
		var failures []error
		for index := len(rollbacks) - 1; index >= 0; index-- {
			item := rollbacks[index]
			if item.existed {
				source, sourceErr := ResolveWithin(rollbackRoot, filepath.FromSlash(item.backup))
				if sourceErr == nil {
					sourceErr = copyRegularFileToRootNoFollow(source, instancePath, item.target, secureWriteOptions{Mode: 0o640, Replace: true, RejectExistingLink: true})
				}
				if sourceErr != nil {
					failures = append(failures, sourceErr)
				}
			} else if deleteErr := deleteManagedFileNoFollow(instancePath, item.target); deleteErr != nil && !errors.Is(deleteErr, os.ErrNotExist) {
				failures = append(failures, deleteErr)
			}
		}
		return errors.Join(failures...)
	}
	progress(60, "installing recognized plugin files")
	for _, item := range pending {
		if err := copyRegularFileToRootNoFollow(item.source, instancePath, item.target, secureWriteOptions{Mode: 0o640, ExpectedSHA256: item.file.SHA256, Replace: true, RejectExistingLink: true}); err != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				return nil, fmt.Errorf("install smart plugin file: %w; rollback also failed: %v", err, rollbackErr)
			}
			return nil, fmt.Errorf("install smart plugin file and rolled back: %w", err)
		}
	}
	progress(82, "committing managed plugin manifests")
	results := make([]map[string]any, 0, len(packageIDs))
	type stateCommit struct{ root, old string }
	stateCommits := make([]stateCommit, 0, len(packageIDs))
	restoreStateCommits := func() error {
		var failures []error
		for index := len(stateCommits) - 1; index >= 0; index-- {
			commit := stateCommits[index]
			if err := os.RemoveAll(commit.root); err != nil {
				failures = append(failures, err)
				continue
			}
			if commit.old != "" {
				if err := os.Rename(commit.old, commit.root); err != nil {
					failures = append(failures, err)
				}
			}
		}
		return errors.Join(failures...)
	}
	for _, folder := range plan.Folders {
		packageID := packageIDs[folder.ID]
		if packageID == "" {
			continue
		}
		state := smartPluginState{Version: 1, PackageID: packageID, InstanceID: job.InstanceID, Name: folder.Name, Enabled: true, ArchiveSHA: archiveSHA, UpdatedAt: time.Now().UTC()}
		aggregate := sha256.New()
		for _, item := range pending {
			if item.folderID != folder.ID {
				continue
			}
			baseline, baselineSHA := item.existingSHA != "" && item.takeover, item.existingSHA
			if _, updating := updateStates[packageID]; updating {
				baseline, baselineSHA = item.baseline, item.baselineSHA
			}
			state.Files = append(state.Files, smartPluginStateFile{ID: item.stateFileID, TargetPath: item.target, SHA256: item.file.SHA256, Size: item.file.Size, Entrypoint: item.file.Entrypoint, Baseline: baseline, BaselineSHA256: baselineSHA})
			_, _ = io.WriteString(aggregate, item.target+"\x00"+item.file.SHA256+"\n")
		}
		state.AggregateSHA = hex.EncodeToString(aggregate.Sum(nil))
		oldState, stateErr := e.writeSmartPluginState(state, rollbackRoot, rollbacks, updateRoots[packageID])
		if stateErr != nil {
			stateRollbackErr := restoreStateCommits()
			fileRollbackErr := rollback()
			if stateRollbackErr != nil || fileRollbackErr != nil {
				return nil, fmt.Errorf("persist smart plugin state: %w; state rollback: %v; file rollback: %v", stateErr, stateRollbackErr, fileRollbackErr)
			}
			return nil, fmt.Errorf("persist smart plugin state and rolled back: %w", stateErr)
		}
		stateRoot, _ := e.smartPluginStateRoot(job.InstanceID, packageID)
		stateCommits = append(stateCommits, stateCommit{root: stateRoot, old: oldState})
		fileResults := make([]map[string]any, 0, len(state.Files))
		for _, file := range state.Files {
			fileResults = append(fileResults, map[string]any{"id": file.ID, "path": file.TargetPath, "sha256": file.SHA256, "size": file.Size, "entrypoint": file.Entrypoint})
		}
		results = append(results, map[string]any{"package_id": packageID, "name": folder.Name, "kind": "smart", "sha256": state.AggregateSHA, "files": fileResults, "file_count": len(fileResults), "native_risk": folder.NativeRisk, "enabled": true})
	}
	for _, commit := range stateCommits {
		if commit.old != "" {
			_ = os.RemoveAll(commit.old)
		}
	}
	progress(100, "smart plugin archive installed")
	return map[string]any{"kind": "smart", "archive_sha256": archiveSHA, "packages": results, "restart_required": running}, nil
}

func copyManagedFileBetweenRoots(sourceRoot, sourceRelative, targetRoot, targetRelative string) error {
	file, err := openManagedFileNoFollow(sourceRoot, sourceRelative)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("smart plugin rollback source is not a regular file")
	}
	_, err = writeFileWithinNoFollow(targetRoot, targetRelative, file, secureWriteOptions{Mode: 0o600, ExpectedSize: info.Size(), Replace: true, RejectExistingLink: true})
	return err
}

func (e *Executor) smartPluginStateRoot(instanceID, packageID string) (string, error) {
	if uuid.Validate(packageID) != nil || strings.TrimSpace(instanceID) == "" {
		return "", errors.New("smart plugin state identifier is invalid")
	}
	root, err := ensureDataSubdirectory(e.config.DataDir, "smart-plugins")
	if err != nil {
		return "", err
	}
	return ResolveWithin(root, filepath.Join(safeLogName(instanceID), packageID))
}

func (e *Executor) writeSmartPluginState(state smartPluginState, rollbackRoot string, rollbacks []smartRollbackFile, oldStateRoot string) (string, error) {
	stateRoot, err := e.smartPluginStateRoot(state.InstanceID, state.PackageID)
	if err != nil {
		return "", err
	}
	temporary := stateRoot + ".tmp-" + uuid.NewString()
	if err := os.MkdirAll(filepath.Join(temporary, "baseline"), 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	byTarget := make(map[string]smartRollbackFile, len(rollbacks))
	for _, item := range rollbacks {
		byTarget[item.target] = item
	}
	for _, file := range state.Files {
		if !file.Baseline {
			continue
		}
		item := byTarget[file.TargetPath]
		if !item.existed {
			return "", errors.New("smart plugin baseline disappeared before commit")
		}
		sourceRoot, sourceRelative := rollbackRoot, item.backup
		if oldStateRoot != "" {
			sourceRoot, sourceRelative = oldStateRoot, filepath.ToSlash(filepath.Join("baseline", file.ID+filepath.Ext(file.TargetPath)))
		}
		source, err := ResolveWithin(sourceRoot, filepath.FromSlash(sourceRelative))
		if err != nil {
			return "", err
		}
		if err := copyRegularFileToRootNoFollow(source, temporary, filepath.ToSlash(filepath.Join("baseline", file.ID+filepath.Ext(file.TargetPath))), secureWriteOptions{Mode: 0o600, Replace: false, RejectExistingLink: true}); err != nil {
			return "", err
		}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", err
	}
	if _, err := writeFileWithinNoFollow(temporary, "manifest.json", strings.NewReader(string(raw)), secureWriteOptions{Mode: 0o600, ExpectedSize: int64(len(raw)), Replace: false, RejectExistingLink: true}); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(stateRoot), 0o700); err != nil {
		return "", err
	}
	old := stateRoot + ".old-" + uuid.NewString()
	if _, statErr := os.Stat(stateRoot); statErr == nil {
		if err := os.Rename(stateRoot, old); err != nil {
			return "", err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	} else {
		old = ""
	}
	if err := os.Rename(temporary, stateRoot); err != nil {
		if old != "" {
			_ = os.Rename(old, stateRoot)
		}
		return "", err
	}
	return old, nil
}

func (e *Executor) readSmartPluginState(instanceID, packageID string) (smartPluginState, string, error) {
	root, err := e.smartPluginStateRoot(instanceID, packageID)
	if err != nil {
		return smartPluginState{}, "", err
	}
	raw, err := readRegularFileWithinNoFollow(root, "manifest.json", 4<<20)
	if err != nil {
		return smartPluginState{}, "", err
	}
	var state smartPluginState
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil || state.Version != 1 || state.InstanceID != instanceID || state.PackageID != packageID || len(state.Files) == 0 || len(state.Files) > maxSmartPluginFiles {
		return smartPluginState{}, "", errors.New("smart plugin state is invalid")
	}
	seen := make(map[string]bool, len(state.Files))
	for _, file := range state.Files {
		if file.ID == "" || seen[file.ID] || file.Size < 0 || len(file.SHA256) != 64 || validateSmartPluginTarget(file.TargetPath, strings.ToLower(filepath.Ext(file.TargetPath))) != nil {
			return smartPluginState{}, "", errors.New("smart plugin state contains an invalid file")
		}
		seen[file.ID] = true
	}
	return state, root, nil
}

func (e *Executor) updateSmartPluginManifest(root string, state smartPluginState) error {
	state.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	_, err = writeFileWithinNoFollow(root, "manifest.json", strings.NewReader(string(raw)), secureWriteOptions{Mode: 0o600, ExpectedSize: int64(len(raw)), Replace: true, RejectExistingLink: true})
	return err
}

func (e *Executor) editSmartPluginFile(job domain.Job, instancePath, relative, content, contentBase64, expectedSHA256 string, progress ProgressFunc) (map[string]any, error) {
	packageID := stringFromPayload(job.Payload, "package_id")
	state, stateRoot, err := e.readSmartPluginState(job.InstanceID, packageID)
	if err != nil {
		return nil, fmt.Errorf("load smart plugin state: %w", err)
	}
	fileIndex := -1
	for index, file := range state.Files {
		if file.TargetPath == relative {
			fileIndex = index
			break
		}
	}
	if fileIndex < 0 || state.Files[fileIndex].Entrypoint || !editableSmartPluginExtensions[strings.ToLower(filepath.Ext(relative))] {
		return nil, errors.New("smart plugin file is not an editable managed text file")
	}
	managed := state.Files[fileIndex]
	if len(expectedSHA256) != sha256.Size*2 || !strings.EqualFold(expectedSHA256, managed.SHA256) {
		return nil, errors.New("smart plugin file precondition does not match its manifest")
	}
	if _, err := hex.DecodeString(expectedSHA256); err != nil {
		return nil, errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	oldRaw, err := readRegularFileWithinNoFollow(instancePath, relative, maxManagedFileSize)
	if err != nil {
		return nil, err
	}
	oldDigest := sha256.Sum256(oldRaw)
	if !strings.EqualFold(hex.EncodeToString(oldDigest[:]), managed.SHA256) {
		return nil, errors.New("smart plugin file changed since its manifest was committed")
	}
	raw := []byte(content)
	if contentBase64 != "" {
		raw, err = base64.StdEncoding.DecodeString(contentBase64)
		if err != nil {
			return nil, errors.New("invalid base64 file content")
		}
	}
	if len(raw) > maxManagedFileSize || bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return nil, errors.New("editable smart plugin text must be valid UTF-8 without NUL bytes and no larger than 8 MiB")
	}
	newDigest := sha256.Sum256(raw)
	newSHA := hex.EncodeToString(newDigest[:])
	if err := writeRawFileWithinNoFollow(instancePath, relative, raw, 0o640, true); err != nil {
		return nil, err
	}
	state.Files[fileIndex].SHA256 = newSHA
	state.Files[fileIndex].Size = int64(len(raw))
	aggregate := sha256.New()
	for _, file := range state.Files {
		_, _ = io.WriteString(aggregate, file.TargetPath+"\x00"+file.SHA256+"\n")
	}
	state.AggregateSHA = hex.EncodeToString(aggregate.Sum(nil))
	if err := e.updateSmartPluginManifest(stateRoot, state); err != nil {
		if rollbackErr := writeRawFileWithinNoFollow(instancePath, relative, oldRaw, 0o640, true); rollbackErr != nil {
			return nil, fmt.Errorf("update smart plugin manifest: %w; restore edited file: %v", err, rollbackErr)
		}
		return nil, fmt.Errorf("update smart plugin manifest and restored edited file: %w", err)
	}
	progress(100, "smart plugin text file updated")
	return map[string]any{"package_id": packageID, "path": relative, "previous_sha256": managed.SHA256,
		"sha256": newSHA, "size": len(raw), "aggregate_sha256": state.AggregateSHA}, nil
}

func (e *Executor) readSmartPluginFile(job domain.Job, instancePath, relative string, progress ProgressFunc) (map[string]any, error) {
	packageID := stringFromPayload(job.Payload, "package_id")
	state, _, err := e.readSmartPluginState(job.InstanceID, packageID)
	if err != nil {
		return nil, fmt.Errorf("load smart plugin state: %w", err)
	}
	var managed *smartPluginStateFile
	for index := range state.Files {
		if state.Files[index].TargetPath == relative {
			managed = &state.Files[index]
			break
		}
	}
	if managed == nil || managed.Entrypoint || !editableSmartPluginExtensions[strings.ToLower(filepath.Ext(relative))] {
		return nil, errors.New("smart plugin file is not an editable managed text file")
	}
	expectedSHA256 := stringFromPayload(job.Payload, "expected_sha256")
	if len(expectedSHA256) != sha256.Size*2 || !strings.EqualFold(expectedSHA256, managed.SHA256) {
		return nil, errors.New("smart plugin file precondition does not match its manifest")
	}
	raw, err := readRegularFileWithinNoFollow(instancePath, relative, maxManagedFileSize)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return nil, errors.New("managed smart plugin file is not valid UTF-8 text")
	}
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	if !strings.EqualFold(sha, managed.SHA256) {
		return nil, errors.New("smart plugin file changed since its manifest was committed")
	}
	progress(100, "smart plugin text file read")
	return map[string]any{"package_id": packageID, "path": relative, "content_base64": base64.StdEncoding.EncodeToString(raw), "size": len(raw), "sha256": sha}, nil
}

func (e *Executor) toggleSmartPlugin(job domain.Job, payload operationPayload, progress ProgressFunc) (map[string]any, error) {
	if !payload.Enabled && e.smartPluginServerRunning(job.InstanceID) {
		return nil, errors.New("instance must be stopped before disabling a smart plugin")
	}
	if payload.Enabled && e.smartPluginServerRunning(job.InstanceID) && (!payload.AcknowledgeLiveChanges || !payload.AcknowledgeRestartRequired) {
		return nil, errors.New("running-instance smart plugin enable requires both live-change acknowledgements")
	}
	state, stateRoot, err := e.readSmartPluginState(job.InstanceID, stringFromPayload(job.Payload, "package_id"))
	if err != nil {
		return nil, fmt.Errorf("load smart plugin state: %w", err)
	}
	if state.Enabled == payload.Enabled {
		return map[string]any{"package_id": state.PackageID, "enabled": state.Enabled, "restart_required": false}, nil
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	moved := make([]smartPluginStateFile, 0)
	rollback := func() {
		for index := len(moved) - 1; index >= 0; index-- {
			file := moved[index]
			disabled := filepath.ToSlash(filepath.Join("disabled", file.ID+filepath.Ext(file.TargetPath)))
			if payload.Enabled {
				_ = moveSmartFileBetweenRoots(instancePath, file.TargetPath, stateRoot, disabled)
			} else {
				_ = moveSmartFileBetweenRoots(stateRoot, disabled, instancePath, file.TargetPath)
			}
		}
	}
	for _, file := range state.Files {
		if !file.Entrypoint {
			continue
		}
		disabled := filepath.ToSlash(filepath.Join("disabled", file.ID+filepath.Ext(file.TargetPath)))
		sourceRoot, sourcePath, targetRoot, targetPath := instancePath, file.TargetPath, stateRoot, disabled
		if payload.Enabled {
			sourceRoot, sourcePath, targetRoot, targetPath = stateRoot, disabled, instancePath, file.TargetPath
		}
		sha, _, err := hashRegularFileWithinNoFollow(sourceRoot, sourcePath, maxSmartPluginEntry)
		if err != nil || !strings.EqualFold(sha, file.SHA256) {
			rollback()
			return nil, fmt.Errorf("smart plugin entrypoint %q changed or is missing", file.TargetPath)
		}
		if err := moveSmartFileBetweenRoots(sourceRoot, sourcePath, targetRoot, targetPath); err != nil {
			rollback()
			return nil, fmt.Errorf("toggle smart plugin entrypoint %q: %w", file.TargetPath, err)
		}
		moved = append(moved, file)
	}
	if len(moved) == 0 {
		return nil, errors.New("smart plugin has no recognized load entrypoints")
	}
	state.Enabled = payload.Enabled
	if err := e.updateSmartPluginManifest(stateRoot, state); err != nil {
		rollback()
		return nil, err
	}
	progress(100, "smart plugin state updated")
	return map[string]any{"package_id": state.PackageID, "enabled": state.Enabled, "restart_required": payload.Enabled && e.smartPluginServerRunning(job.InstanceID)}, nil
}

func moveSmartFileBetweenRoots(sourceRoot, sourceRelative, targetRoot, targetRelative string) error {
	source, err := openManagedFileNoFollow(sourceRoot, sourceRelative)
	if err != nil {
		return err
	}
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = source.Close()
		return errors.New("smart plugin move source is not a regular file")
	}
	if _, err := writeFileWithinNoFollow(targetRoot, targetRelative, source, secureWriteOptions{Mode: 0o640, ExpectedSize: info.Size(), Replace: false, RejectExistingLink: true}); err != nil {
		_ = source.Close()
		return err
	}
	if err := source.Close(); err != nil {
		_ = deleteManagedFileNoFollow(targetRoot, targetRelative)
		return err
	}
	if err := deleteManagedFileNoFollow(sourceRoot, sourceRelative); err != nil {
		_ = deleteManagedFileNoFollow(targetRoot, targetRelative)
		return err
	}
	return nil
}

func (e *Executor) deleteSmartPlugin(job domain.Job, progress ProgressFunc) (map[string]any, error) {
	if e.smartPluginServerRunning(job.InstanceID) {
		return nil, errors.New("instance must be stopped before deleting a smart plugin")
	}
	packageID := stringFromPayload(job.Payload, "package_id")
	state, stateRoot, err := e.readSmartPluginState(job.InstanceID, packageID)
	if err != nil {
		return nil, fmt.Errorf("load smart plugin state: %w", err)
	}
	record, ok := e.registry.Get(job.InstanceID)
	if !ok {
		return nil, errors.New("instance is not registered")
	}
	instancePath, err := e.instancePath(record)
	if err != nil {
		return nil, err
	}
	for _, file := range state.Files {
		sourceRoot, sourcePath := instancePath, file.TargetPath
		if !state.Enabled && file.Entrypoint {
			sourceRoot, sourcePath = stateRoot, filepath.ToSlash(filepath.Join("disabled", file.ID+filepath.Ext(file.TargetPath)))
		}
		sha, _, err := hashRegularFileWithinNoFollow(sourceRoot, sourcePath, maxSmartPluginEntry)
		if err != nil || !strings.EqualFold(sha, file.SHA256) {
			return nil, fmt.Errorf("smart plugin file %q changed or is missing; deletion was refused", file.TargetPath)
		}
	}
	for _, file := range state.Files {
		sourceRoot, sourcePath := instancePath, file.TargetPath
		if !state.Enabled && file.Entrypoint {
			sourceRoot, sourcePath = stateRoot, filepath.ToSlash(filepath.Join("disabled", file.ID+filepath.Ext(file.TargetPath)))
		}
		if file.Baseline {
			baseline := filepath.ToSlash(filepath.Join("baseline", file.ID+filepath.Ext(file.TargetPath)))
			baselinePath, err := ResolveWithin(stateRoot, filepath.FromSlash(baseline))
			if err != nil {
				return nil, err
			}
			if err := copyRegularFileToRootNoFollow(baselinePath, instancePath, file.TargetPath, secureWriteOptions{Mode: 0o640, ExpectedSHA256: file.BaselineSHA256, Replace: true, RejectExistingLink: true}); err != nil {
				return nil, fmt.Errorf("restore taken-over file %q: %w", file.TargetPath, err)
			}
			if sourceRoot != instancePath {
				_ = deleteManagedFileNoFollow(sourceRoot, sourcePath)
			}
		} else if err := deleteManagedFileNoFollow(sourceRoot, sourcePath); err != nil {
			return nil, fmt.Errorf("delete smart plugin file %q: %w", file.TargetPath, err)
		}
	}
	if err := os.RemoveAll(stateRoot); err != nil {
		return nil, fmt.Errorf("remove smart plugin state: %w", err)
	}
	progress(100, "smart plugin deleted")
	return map[string]any{"package_id": packageID, "sha256": state.AggregateSHA, "files": len(state.Files)}, nil
}

func (e *Executor) smartPluginServerRunning(instanceID string) bool {
	if e.packageServerRunning(instanceID) {
		return true
	}
	if e.registry == nil {
		return false
	}
	record, ok := e.registry.Get(instanceID)
	return ok && record.ExternalPID > 0
}

func stringFromPayload(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}
