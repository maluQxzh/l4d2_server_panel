package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type smartQueueFile struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Entrypoint bool   `json:"entrypoint"`
}

type smartQueueFolder struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	NativeRisk bool   `json:"native_risk"`
	Files      []struct {
		ID         string `json:"id"`
		SourcePath string `json:"source_path"`
		TargetPath string `json:"target_path"`
		SHA256     string `json:"sha256"`
		Size       int64  `json:"size"`
		Entrypoint bool   `json:"entrypoint"`
		Native     bool   `json:"native"`
		PathStates map[string]struct {
			Existing         bool   `json:"existing"`
			ExistingSHA256   string `json:"existing_sha256"`
			ExistingSize     *int64 `json:"existing_size"`
			ManagedPackageID string `json:"managed_package_id"`
		} `json:"path_states"`
	} `json:"files"`
}

type smartQueuePlan struct {
	Version       int                `json:"version"`
	ArchiveName   string             `json:"archive_name"`
	ArchiveSHA256 string             `json:"archive_sha256"`
	PathEncoding  string             `json:"path_encoding"`
	FileCount     int                `json:"file_count"`
	ExpandedBytes int64              `json:"expanded_bytes"`
	Folders       []smartQueueFolder `json:"folders"`
}

type smartQueueExpected struct {
	Name       string                    `json:"name"`
	PackageID  string                    `json:"package_id"`
	NativeRisk bool                      `json:"native_risk"`
	Files      map[string]smartQueueFile `json:"files"`
}

func preparedSmartPlan(inspection domain.Job) (smartQueuePlan, error) {
	var plan smartQueuePlan
	raw, err := json.Marshal(inspection.Result)
	if err != nil {
		return plan, err
	}
	invalid := fmt.Errorf("智能插件预检清单无效: %w", ErrConflict)
	if json.Unmarshal(raw, &plan) != nil || plan.Version != 2 || plan.ArchiveName == "" || len(plan.ArchiveName) > 255 || !validPackageSHA(plan.ArchiveSHA256) || !domain.ValidMapPathEncoding(plan.PathEncoding) || len(plan.Folders) == 0 || len(plan.Folders) > 256 || plan.ExpandedBytes < 0 || plan.ExpandedBytes > 2<<30 {
		return plan, invalid
	}
	seen := map[string]bool{}
	count := 0
	var size int64
	for _, folder := range plan.Folders {
		if folder.ID == "" || len(folder.ID) > 128 || seen[folder.ID] || folder.Name == "" || len(folder.Name) > 255 || len(folder.Files) == 0 {
			return plan, invalid
		}
		seen[folder.ID] = true
		files := map[string]bool{}
		native := false
		for _, file := range folder.Files {
			count++
			size += file.Size
			if count > 5000 || file.ID == "" || len(file.ID) > 128 || files[file.ID] || file.SourcePath == "" || !validPackageSHA(file.SHA256) || file.Size < 0 || file.Size > 512<<20 {
				return plan, invalid
			}
			files[file.ID] = true
			ext := strings.ToLower(filepath.Ext(file.SourcePath))
			native = native || file.Native || ext == ".dll" || ext == ".so"
		}
		if native != folder.NativeRisk {
			return plan, invalid
		}
	}
	if count != plan.FileCount || size > plan.ExpandedBytes {
		return plan, invalid
	}
	return plan, nil
}

// Called inside the projection transaction. The immutable child carries the
// selected archive files and the previous generated config snapshot.
func validatePreparedSmartResultTx(ctx context.Context, tx *sql.Tx, job domain.Job) error {
	invalid := errors.New("smart plugin result does not match its immutable inspection")
	inspection, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1`, stringFromAny(job.Payload["prepared_package_id"])))
	if err != nil {
		return err
	}
	if inspection.Type != "inspect_plugin_archive" || inspection.Status != domain.JobSucceeded || inspection.NodeID != job.NodeID || inspection.InstanceID != job.InstanceID || stringFromAny(inspection.Result["preview_id"]) != stringFromAny(job.Payload["smart_preview_id"]) || stringFromAny(inspection.Result["fingerprint"]) != stringFromAny(job.Payload["inspection_fingerprint"]) || stringFromAny(job.Result["kind"]) != "smart" || stringFromAny(job.Result["archive_sha256"]) != stringFromAny(inspection.Result["archive_sha256"]) {
		return invalid
	}
	var expected map[string]smartQueueExpected
	raw, _ := json.Marshal(job.Payload["smart_expected_packages"])
	if json.Unmarshal(raw, &expected) != nil || len(expected) == 0 || len(expected) > 256 {
		return invalid
	}
	byID := map[string]smartQueueExpected{}
	for folder, item := range expected {
		id := item.PackageID
		if id == "" {
			id = uuid.NewSHA1(uuid.NameSpaceOID, []byte(job.ID+":"+folder)).String()
		}
		byID[id] = item
	}
	var result struct {
		Packages []struct {
			PackageID  string           `json:"package_id"`
			Name       string           `json:"name"`
			SHA256     string           `json:"sha256"`
			NativeRisk bool             `json:"native_risk"`
			Files      []smartQueueFile `json:"files"`
		} `json:"packages"`
	}
	raw, _ = json.Marshal(job.Result)
	if json.Unmarshal(raw, &result) != nil || len(result.Packages) != len(byID) {
		return invalid
	}
	for _, item := range result.Packages {
		want, ok := byID[item.PackageID]
		if !ok || item.Name != want.Name || item.NativeRisk != want.NativeRisk || len(item.Files) != len(want.Files) {
			return invalid
		}
		delete(byID, item.PackageID)
		aggregate := sha256.New()
		for _, file := range item.Files {
			wantFile, ok := want.Files[file.Path]
			if !ok || file != wantFile {
				return invalid
			}
			delete(want.Files, file.Path)
			_, _ = aggregate.Write([]byte(file.Path + "\x00" + file.SHA256 + "\n"))
		}
		if item.SHA256 != hex.EncodeToString(aggregate.Sum(nil)) {
			return invalid
		}
	}
	return nil
}

// preparedSmartConsumptionPayload turns a completed archive inspection into
// an install child without retaining original source fields.
// The browser supplies only folder choices and confirmations;
// current ownership is read again below in the same queue transaction.
func preparedSmartConsumptionPayload(ctx context.Context, tx *sql.Tx, instanceID, action string, payload map[string]any, inspection domain.Job) (map[string]any, error) {
	expires, expiryErr := time.Parse(time.RFC3339Nano, stringFromAny(inspection.Result["preview_expires_at"]))
	if expiryErr != nil || !expires.After(time.Now()) || expires.After(inspection.CreatedAt.Add(2*time.Hour)) || action != "install_mod" || inspection.Status != domain.JobSucceeded || inspection.CreatedAt.Add(time.Hour).Before(time.Now().UTC()) {
		return nil, fmt.Errorf("智能插件预检已过期或未完成: %w", ErrConflict)
	}
	previewID := stringFromAny(inspection.Result["preview_id"])
	fingerprint := strings.ToLower(strings.TrimSpace(stringFromAny(inspection.Result["fingerprint"])))
	if uuid.Validate(previewID) != nil || len(fingerprint) != 64 || strings.Trim(fingerprint, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("智能插件预检句柄无效: %w", ErrConflict)
	}
	rawSelections, ok := payload["smart_packages"]
	if !ok {
		return nil, fmt.Errorf("智能插件队列项缺少文件夹选择: %w", ErrConflict)
	}
	selectionRaw, err := json.Marshal(rawSelections)
	if err != nil {
		return nil, fmt.Errorf("智能插件选择无效: %w", ErrConflict)
	}
	var selections []preparedSmartSelection
	if json.Unmarshal(selectionRaw, &selections) != nil || len(selections) == 0 || len(selections) > 256 {
		return nil, fmt.Errorf("智能插件选择必须包含 1–256 个文件夹: %w", ErrConflict)
	}
	plan, err := preparedSmartPlan(inspection)
	if err != nil {
		return nil, err
	}
	knownFolders := make(map[string]smartQueueFolder, len(plan.Folders))
	for _, folder := range plan.Folders {
		knownFolders[folder.ID] = folder
	}
	seen, packageIDs := map[string]bool{}, map[string]bool{}
	for _, selection := range selections {
		folder, found := knownFolders[selection.FolderID]
		if !found || seen[selection.FolderID] {
			return nil, fmt.Errorf("智能插件选择包含未知或重复文件夹: %w", ErrConflict)
		}
		seen[selection.FolderID] = true
		if selection.PackageID != "" {
			var enabled bool
			if uuid.Validate(selection.PackageID) != nil || packageIDs[selection.PackageID] {
				return nil, fmt.Errorf("智能插件更新包 ID 无效或重复: %w", ErrConflict)
			}
			if err := tx.QueryRowContext(ctx, `SELECT enabled FROM addon_packages WHERE id=?1 AND instance_id=?2 AND kind='smart'`, selection.PackageID, instanceID).Scan(&enabled); err != nil || !enabled {
				return nil, fmt.Errorf("智能插件更新目标不存在或已停用: %w", ErrConflict)
			}
			packageIDs[selection.PackageID] = true
		}
		if folder.NativeRisk && !boolFromAny(payload["acknowledge_native_risk"]) {
			return nil, fmt.Errorf("原生插件安装必须确认第三方代码风险: %w", ErrConflict)
		}
		files := map[string]bool{}
		for _, file := range folder.Files {
			files[file.ID] = true
		}
		for id := range selection.Overrides {
			if !files[id] {
				return nil, fmt.Errorf("未知路径选择: %w", ErrConflict)
			}
		}
		takeovers := map[string]bool{}
		for _, id := range selection.Takeovers {
			if !files[id] || takeovers[id] {
				return nil, fmt.Errorf("未知或重复接管选择: %w", ErrConflict)
			}
			takeovers[id] = true
		}
		for _, id := range selection.PreserveExisting {
			if !files[id] || takeovers[id] || selection.PackageID != "" {
				return nil, fmt.Errorf("未知、重复或冲突的保留内容接管选择: %w", ErrConflict)
			}
			takeovers[id] = true
		}
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM instances WHERE id=?1`, instanceID).Scan(&status); err != nil {
		return nil, err
	}
	live, _ := payload["acknowledge_live_changes"].(bool)
	restart, _ := payload["acknowledge_restart_required"].(bool)
	if (status == string(domain.InstanceRunning) || status == string(domain.InstanceDegraded) || status == string(domain.InstanceExternal)) && (!live || !restart) {
		return nil, fmt.Errorf("运行中智能插件安装必须确认实时变更和重启: %w", ErrConflict)
	}
	registered := make(map[string]string)
	rows, err := tx.QueryContext(ctx, `SELECT package_id,path FROM addon_package_files WHERE instance_id=?1 UNION ALL SELECT id,path FROM addon_packages WHERE instance_id=?1`, instanceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var owner, pathValue string
		if err := rows.Scan(&owner, &pathValue); err != nil {
			rows.Close()
			return nil, err
		}
		if pathValue != "" {
			registered[strings.ToLower(filepath.ToSlash(pathValue))] = owner
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	expected := make(map[string]smartQueueExpected)
	targets := map[string]bool{}
	for _, selection := range selections {
		folder := knownFolders[selection.FolderID]
		item := smartQueueExpected{Name: folder.Name, PackageID: selection.PackageID, NativeRisk: folder.NativeRisk, Files: map[string]smartQueueFile{}}
		// Preserve the currently registered generated CFGs when updating a plugin.
		// The Agent independently requires the original managed path set.
		if selection.PackageID != "" {
			rows, err := tx.QueryContext(ctx, `SELECT path,sha256,size,entrypoint FROM addon_package_files WHERE package_id=?1 AND instance_id=?2 ORDER BY path LIMIT 5001`, selection.PackageID, instanceID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var file smartQueueFile
				if err := rows.Scan(&file.Path, &file.SHA256, &file.Size, &file.Entrypoint); err != nil {
					rows.Close()
					return nil, err
				}
				item.Files[file.Path] = file
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if len(item.Files) == 0 || len(item.Files) > 5000 {
				return nil, fmt.Errorf("智能插件原清单无效: %w", ErrConflict)
			}
		}
		selectedPaths := map[string]bool{}
		preserved := map[string]bool{}
		for _, id := range selection.PreserveExisting {
			preserved[id] = true
		}
		for _, file := range folder.Files {
			target := file.TargetPath
			if override := strings.TrimSpace(selection.Overrides[file.ID]); override != "" {
				target = override
			}
			if validateSmartPluginResultPath(target) != nil || !strings.EqualFold(filepath.Ext(target), filepath.Ext(file.SourcePath)) {
				return nil, fmt.Errorf("智能插件目标路径无效: %w", ErrConflict)
			}
			key := strings.ToLower(target)
			if targets[key] || registered[key] != "" && registered[key] != selection.PackageID {
				return nil, fmt.Errorf("智能插件目标重复或已由其他包管理: %w", ErrConflict)
			}
			targets[key], selectedPaths[target] = true, true
			if selection.PackageID != "" {
				if _, ok := item.Files[target]; !ok {
					return nil, fmt.Errorf("智能插件更新不能增删或移动原文件: %w", ErrConflict)
				}
			}
			item.Files[target] = smartQueueFile{Path: target, SHA256: file.SHA256, Size: file.Size, Entrypoint: file.Entrypoint}
			if preserved[file.ID] {
				observed, found := file.PathStates[target]
				if !found || !observed.Existing || observed.ManagedPackageID != "" || !validPackageSHA(observed.ExistingSHA256) || observed.ExistingSize == nil || *observed.ExistingSize < 0 || *observed.ExistingSize > 512<<20 {
					return nil, fmt.Errorf("接管但不覆盖需要预检中的未纳管普通文件，请重新识别: %w", ErrConflict)
				}
				item.Files[target] = smartQueueFile{Path: target, SHA256: observed.ExistingSHA256, Size: *observed.ExistingSize, Entrypoint: file.Entrypoint}
			}
		}
		for path, file := range item.Files {
			if selectedPaths[path] {
				continue
			}
			// Only existing non-entrypoint generated-config candidates may be retained
			// outside the archive. Agent state distinguishes generated from packaged CFGs.
			if _, ok := generatedConfigManagedCFGBase(path); !ok || file.Entrypoint {
				return nil, fmt.Errorf("智能插件更新必须保留原文件集合: %w", ErrConflict)
			}
		}
		expected[folder.ID] = item
	}
	result := map[string]any{
		"prepared_package_id": inspection.ID,
		"smart_preview_id":    previewID,
		"kind":                "smart", "name": stringFromAny(inspection.Result["archive_name"]),
		"inspection_fingerprint": fingerprint, "smart_packages": selections,
		"acknowledge_native_risk":  boolFromAny(payload["acknowledge_native_risk"]),
		"risk_confirmed":           boolFromAny(payload["acknowledge_native_risk"]),
		"acknowledge_live_changes": live, "acknowledge_restart_required": restart,
		"registered_paths": registered, "smart_expected_packages": expected,
	}
	result["path_encoding"] = inspection.Result["path_encoding"]
	result["expected_sha256"] = inspection.Result["archive_sha256"]
	return result, nil
}
