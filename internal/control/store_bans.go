package control

import (
	"context"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (s *Store) ListPlayerBans(ctx context.Context, instanceID string) ([]domain.PlayerBan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,instance_id,steam_id,COALESCE(ip_address,''),reason,
		COALESCE(created_by,''),expires_at,created_at FROM player_bans
		WHERE instance_id=?1 AND (expires_at IS NULL OR expires_at>?2) ORDER BY created_at DESC,id DESC`, instanceID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.PlayerBan, 0)
	for rows.Next() {
		var item domain.PlayerBan
		if err := rows.Scan(&item.ID, &item.InstanceID, &item.SteamID, &item.IPAddress, &item.Reason,
			&item.CreatedBy, &item.ExpiresAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
