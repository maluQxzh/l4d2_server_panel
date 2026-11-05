package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type jobListCursor struct {
	CreatedAt time.Time `json:"at"`
	ID        string    `json:"id"`
}

type jobListFilter struct {
	Status string
	Search string
	Cursor *jobListCursor
}

type jobListSummary struct {
	Active    int `json:"active"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

func parseJobListFilter(values url.Values) (jobListFilter, error) {
	f := jobListFilter{Status: values.Get("status"), Search: strings.TrimSpace(values.Get("q"))}
	switch f.Status {
	case "", "all", "active", "queued", "dispatched", "running", "succeeded", "failed", "canceled":
	default:
		return f, errors.New("invalid job status")
	}
	if len(f.Search) > 200 {
		return f, errors.New("job search exceeds 200 bytes")
	}
	if raw := values.Get("cursor"); raw != "" {
		if len(raw) > 512 {
			return f, errors.New("invalid job cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(raw)
		var cursor jobListCursor
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.CreatedAt.IsZero() || cursor.ID == "" || len(cursor.ID) > 128 {
			return f, errors.New("invalid job cursor")
		}
		f.Cursor = &cursor
	}
	return f, nil
}

// The same SQL authorization predicate gates rows and totals before filtering
// or pagination. A caller-supplied cursor only narrows this authorized scope.
func jobListScope(nodeID, instanceID string, background jobBackgroundFilter, visibility *jobVisibilityScope) (string, []any, error) {
	where := ` WHERE (?1='' OR node_id=?1) AND (?2='' OR instance_id=?2)`
	args := []any{nodeID, instanceID}
	switch background {
	case jobBackgroundExclude:
		where += ` AND NOT (` + backgroundJobSQLPredicate + `)`
	case jobBackgroundOnly:
		where += ` AND (` + backgroundJobSQLPredicate + `)`
	case jobBackgroundInclude:
	default:
		return "", nil, errors.New("invalid background filter")
	}
	if visibility != nil {
		nodes, _ := json.Marshal(visibility.NodeIDs)
		instances, _ := json.Marshal(visibility.InstanceIDs)
		legacy, _ := json.Marshal(visibility.LegacyOwnNodeIDs)
		args = append(args, string(nodes), string(instances), visibility.RequestedBy, string(legacy))
		where += ` AND (node_id IN (SELECT value FROM json_each(?3))
			OR instance_id IN (SELECT value FROM json_each(?4))
			OR (instance_id IS NULL AND type IN ('map.import.qq_flash','map.verify','map.delete')
				AND json_extract(payload,'$._scope_instance_id') IN (SELECT value FROM json_each(?4)))
			OR (instance_id IS NULL AND requested_by=?5 AND node_id IN (SELECT value FROM json_each(?6))
				AND type IN ('map.import.qq_flash','map.verify','map.delete')
				AND COALESCE(json_extract(payload,'$._scope_instance_id'),'')=''))`
	}
	return where, args, nil
}

func (s *Store) ListJobPage(ctx context.Context, nodeID, instanceID string, background jobBackgroundFilter, visibility *jobVisibilityScope, filter jobListFilter, limit int) ([]domain.Job, string, jobListSummary, error) {
	var summary jobListSummary
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where, args, err := jobListScope(nodeID, instanceID, background, visibility)
	if err != nil {
		return nil, "", summary, err
	}
	if filter.Search != "" {
		args = append(args, filter.Search)
		// Search only public identifiers, never payload/result or redacted text.
		where += fmt.Sprintf(` AND (instr(lower(id),lower(?%d))>0 OR instr(lower(type),lower(?%d))>0)`, len(args), len(args))
	}
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status IN ('queued','dispatched','running')),
		count(*) FILTER (WHERE status='succeeded'),count(*) FILTER (WHERE status='failed') FROM jobs`+where, args...).Scan(&summary.Active, &summary.Succeeded, &summary.Failed)
	if err != nil {
		return nil, "", summary, err
	}
	if filter.Status == "active" {
		where += ` AND status IN ('queued','dispatched','running')`
	} else if filter.Status != "" && filter.Status != "all" {
		args = append(args, filter.Status)
		where += fmt.Sprintf(` AND status=?%d`, len(args))
	}
	if filter.Cursor != nil {
		args = append(args, filter.Cursor.CreatedAt, filter.Cursor.ID)
		where += fmt.Sprintf(` AND (created_at,id)<(?%d,?%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumnsWithRequesterName+` FROM jobs`+where+fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT ?%d`, len(args)), args...)
	if err != nil {
		return nil, "", summary, err
	}
	defer rows.Close()
	items := make([]domain.Job, 0)
	for rows.Next() {
		job, err := scanJobWithRequesterName(rows)
		if err != nil {
			return nil, "", summary, err
		}
		items = append(items, job)
	}
	if err := rows.Err(); err != nil {
		return nil, "", summary, err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		raw, _ := json.Marshal(jobListCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return items, next, summary, nil
}
