package control

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type ConfigVersion struct {
	ID          string                `json:"id"`
	InstanceID  string                `json:"instance_id"`
	Version     int                   `json:"version"`
	Config      domain.InstanceConfig `json:"config"`
	CreatedBy   string                `json:"created_by,omitempty"`
	CreatedName string                `json:"created_name,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	Current     bool                  `json:"current"`
}

func (s *Store) ListConfigVersions(ctx context.Context, instanceID string) ([]ConfigVersion, error) {
	current, err := s.GetInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	items := []ConfigVersion{{
		ID: instanceID + ":current", InstanceID: instanceID, Version: current.Version, Config: current.Config,
		CreatedAt: current.UpdatedAt, Current: true,
	}}
	rows, err := s.pool.Query(ctx, `SELECT version.id,version.instance_id::text,version.version,version.config,
		COALESCE(version.created_by::text,''),COALESCE(users.display_name,users.username,''),version.created_at
		FROM config_versions version LEFT JOIN users ON users.id=version.created_by
		WHERE version.instance_id=$1 ORDER BY version.version DESC,version.created_at DESC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ConfigVersion
		var raw []byte
		if err := rows.Scan(&item.ID, &item.InstanceID, &item.Version, &raw, &item.CreatedBy, &item.CreatedName, &item.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &item.Config); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ConfigAtVersion(ctx context.Context, instanceID string, version int) (domain.InstanceConfig, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT config FROM config_versions WHERE instance_id=$1 AND version=$2`, instanceID, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.InstanceConfig{}, ErrNotFound
	}
	if err != nil {
		return domain.InstanceConfig{}, err
	}
	var config domain.InstanceConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return domain.InstanceConfig{}, err
	}
	return config, nil
}
