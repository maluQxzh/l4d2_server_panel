package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Store) applyPreparedVPKBundleResult(ctx context.Context, job domain.Job) error {
	prepared, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1 AND instance_id=?2 AND node_id=?3 AND type='prepare_package'`, stringFromAny(job.Payload["prepared_package_id"]), job.InstanceID, job.NodeID))
	if err != nil {
		return err
	}
	meta, err := preparedMetadata(prepared)
	if err != nil {
		return err
	}
	if meta.Kind != domain.PreparedVPKBundleKind || meta.SHA256 != stringFromAny(job.Payload["expected_sha256"]) || meta.Size != int64FromAny(job.Payload["expected_size"]) {
		return errors.New("prepared VPK projection metadata mismatch")
	}
	var bundle domain.PreparedVPKBundle
	raw, _ := json.Marshal(prepared.Result)
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return err
	}
	var result struct {
		FilesImported int `json:"files_imported"`
		Packages      []struct {
			domain.PreparedVPKFile
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"packages"`
	}
	raw, err = json.Marshal(job.Result)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return errors.New("prepared VPK projection has invalid field types")
	}
	if len(result.Packages) != len(bundle.Files) || result.FilesImported != len(bundle.Files) {
		return errors.New("prepared VPK projection count mismatch")
	}
	for index, file := range bundle.Files {
		item := result.Packages[index]
		target, _ := domain.PreparedVPKBundleTarget(meta.ID, index, file.Name)
		if item.Path != target || item.Kind != "vpk" || item.PreparedVPKFile != file {
			return errors.New("prepared VPK projection file mismatch")
		}
	}
	return s.applyQQFlashPackageResult(ctx, job)
}
