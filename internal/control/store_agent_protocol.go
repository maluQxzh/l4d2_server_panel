package control

import "context"

// NodeOwnsInstances verifies an entire Agent message before any part of it is
// persisted. IDs are validated and de-duplicated by the protocol layer first.
func (s *Store) NodeOwnsInstances(ctx context.Context, nodeID string, instanceIDs []string) (bool, error) {
	if len(instanceIDs) == 0 {
		return true, nil
	}
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE node_id=$1 AND id::text=ANY($2::text[])`, nodeID, instanceIDs).Scan(&count)
	return count == len(instanceIDs), err
}
