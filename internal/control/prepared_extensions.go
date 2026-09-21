package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func validatePreparedExtensionResult(job domain.Job) error {
	var result struct {
		Kind      string `json:"kind"`
		SHA256    string `json:"sha256"`
		Size      int64  `json:"size"`
		PackageID string `json:"package_id"`
		Verified  bool   `json:"verified"`
		Command   string `json:"verification_command"`
	}
	raw, err := json.Marshal(job.Result)
	if err != nil {
		return err
	}
	command := "meta version"
	if stringFromAny(job.Payload["kind"]) == "sourcemod" {
		command = "sm version"
	}
	if json.Unmarshal(raw, &result) != nil || !result.Verified || result.Command != command || result.Kind != stringFromAny(job.Payload["kind"]) || result.SHA256 != stringFromAny(job.Payload["expected_sha256"]) || result.Size != int64FromAny(job.Payload["expected_size"]) || result.PackageID != stringFromAny(job.Payload["package_id"]) {
		return errors.New("prepared extension result does not match its verified immutable input")
	}
	return nil
}

// Read live ownership in the same immediate transaction that creates the
// installation intent. Preparation never captures dependencies or permissions.
func preparedExtensionInventory(ctx context.Context, tx *sql.Tx, instanceID, kind string, payload map[string]any) error {
	if kind == "sourcemod" {
		var enabled bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM addon_packages WHERE instance_id=?1 AND lower(kind)='metamod' AND enabled=true)`, instanceID).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return fmt.Errorf("安装 SourceMod 前必须先安装并启用 Metamod:Source: %w", ErrConflict)
		}
	}
	var packageID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM addon_packages WHERE instance_id=?1 AND lower(kind)=?2 ORDER BY installed_at DESC,id DESC LIMIT 1`, instanceID, kind).Scan(&packageID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	paths := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT id,path FROM addon_packages WHERE instance_id=?1 AND path<>'' UNION ALL SELECT package_id,path FROM addon_package_files WHERE instance_id=?1 ORDER BY path,id LIMIT 50001`, instanceID)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var owner, path string
		if err = rows.Scan(&owner, &path); err != nil {
			break
		}
		paths[strings.ToLower(path)] = owner
		count++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if count > 50000 {
		return fmt.Errorf("组件文件归属清单过大: %w", ErrConflict)
	}
	files := []map[string]any{}
	rows, err = tx.QueryContext(ctx, `SELECT path,sha256,size FROM addon_package_files WHERE instance_id=?1 AND package_id=?2 ORDER BY path`, instanceID, packageID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var path, sha string
		var size int64
		if err = rows.Scan(&path, &sha, &size); err != nil {
			break
		}
		files = append(files, map[string]any{"path": path, "sha256": sha, "size": size})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	payload["package_id"], payload["registered_paths"], payload["managed_files"] = packageID, paths, files
	return nil
}
