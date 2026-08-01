package control

import (
	"context"
	"encoding/json"
)

// NodeOwnsInstances verifies an entire Agent message before any part of it is
// persisted. IDs are validated and de-duplicated by the protocol layer first.
func (s *Store) NodeOwnsInstances(ctx context.Context, nodeID string, instanceIDs []string) (bool, error) {
	if len(instanceIDs) == 0 {
		return true, nil
	}
	rawIDs, err := json.Marshal(instanceIDs)
	if err != nil {
		return false, err
	}
	var count int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM instances
		WHERE node_id=?1 AND id IN (SELECT value FROM json_each(?2))`, nodeID, string(rawIDs)).Scan(&count)
	return count == len(instanceIDs), err
}
