package control

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Store) applyPreparedWorkshopResult(ctx context.Context, job domain.Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	prepared, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?1 AND instance_id=?2 AND node_id=?3 AND type='prepare_package'`, stringFromAny(job.Payload["prepared_package_id"]), job.InstanceID, job.NodeID))
	if err != nil {
		return err
	}
	meta, err := preparedMetadata(prepared)
	if err != nil {
		return err
	}
	if meta.Kind != domain.PreparedWorkshopKind || meta.SHA256 != stringFromAny(job.Payload["expected_sha256"]) || meta.Size != int64FromAny(job.Payload["expected_size"]) {
		return errors.New("prepared Workshop projection metadata mismatch")
	}
	var bundle domain.PreparedWorkshop
	raw, _ := json.Marshal(prepared.Result)
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return err
	}
	var result struct {
		Kind       string   `json:"kind"`
		SourceID   string   `json:"source_id"`
		SourceIDs  []string `json:"source_ids"`
		Collection bool     `json:"collection"`
		Files      []struct {
			SourceID string `json:"source_id"`
			Path     string `json:"path"`
			SHA256   string `json:"sha256"`
			Size     int64  `json:"size"`
			Version  string `json:"version"`
		} `json:"files"`
	}
	raw, err = json.Marshal(job.Result)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, &result) != nil || result.Kind != "workshop" || result.SourceID != bundle.SourceID || !slices.Equal(result.SourceIDs, bundle.SourceIDs) || result.Collection != bundle.Collection || len(result.Files) != len(bundle.Files) {
		return errors.New("prepared Workshop projection mismatch")
	}
	for index, file := range bundle.Files {
		target, _ := file.Target()
		item := result.Files[index]
		if item.Path != target || item.SourceID != file.SourceID || item.SHA256 != file.SHA256 || item.Size != file.Size || item.Version != file.SHA256[:12] {
			return errors.New("prepared Workshop projection file mismatch")
		}
	}
	for _, file := range bundle.Files {
		target, _ := file.Target()
		packageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(job.ID+":"+target)).String()
		_, err := tx.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,installed_by,last_job_id)
			VALUES(?1,?2,?3,'workshop',?4,?5,?6,?7,?8,true,'normal',?9,?10) ON CONFLICT(id) DO NOTHING`, packageID, job.InstanceID, file.Name, file.SHA256[:12], bundle.SourceID, file.SourceID, file.SHA256, target, nullableUUID(job.RequestedBy), job.ID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
