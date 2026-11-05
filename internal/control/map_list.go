package control

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// Reuse the timestamp/ID cursor format; authorization is always reapplied.
func parseMapCursor(raw string) (*jobListCursor, error) {
	f, err := parseJobListFilter(url.Values{"cursor": {raw}})
	return f.Cursor, err
}

func (s *Store) ListMapPage(ctx context.Context, nodeID string, allowedNodes []string, cursor *jobListCursor, limit int) ([]domain.Map, string, error) {
	if limit < 1 || limit > 500 {
		limit = 200
	}
	where := ` WHERE (?1='' OR node_id=?1)`
	args := []any{nodeID}
	if allowedNodes != nil {
		raw, _ := json.Marshal(allowedNodes)
		args = append(args, string(raw))
		where += ` AND node_id IN (SELECT value FROM json_each(?2))`
	}
	if cursor != nil {
		args = append(args, cursor.CreatedAt, cursor.ID)
		where += fmt.Sprintf(` AND (created_at,id)<(?%d,?%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	// Close the base rows before hydration, so parallel lists never exhaust
	// the connection pool while each waits for a second connection.
	rows, err := s.db.QueryContext(ctx, `SELECT `+mapColumns+` FROM maps`+where+fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT ?%d`, len(args)), args...)
	if err != nil {
		return nil, "", err
	}
	items := make([]domain.Map, 0)
	for rows.Next() {
		item, err := scanMap(rows)
		if err != nil {
			rows.Close()
			return nil, "", err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		raw, _ := json.Marshal(jobListCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err := s.hydrateMaps(ctx, items); err != nil {
		return nil, "", err
	}
	return items, next, nil
}

func (s *Store) hydrateMaps(ctx context.Context, items []domain.Map) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	byID := make(map[string]*domain.Map, len(items))
	for i := range items {
		ids[i] = items[i].ID
		byID[items[i].ID] = &items[i]
	}
	raw, _ := json.Marshal(ids)
	read := func(statement string, scan func(*sql.Rows) error) error {
		rows, err := s.db.QueryContext(ctx, statement, string(raw))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			// Each scanner reads map_id first and resolves it using byID below.
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	lookup := func(id string) (*domain.Map, error) {
		m := byID[id]
		if m == nil {
			return nil, errors.New("map hydration returned an unexpected ID")
		}
		return m, nil
	}
	if err := read(`SELECT map_id,id,ordinal,sha256,size FROM map_artifacts WHERE map_id IN (SELECT value FROM json_each(?1)) ORDER BY map_id,ordinal`, func(rows *sql.Rows) error {
		var a domain.MapArtifact
		if err := rows.Scan(&a.MapID, &a.ID, &a.Ordinal, &a.SHA256, &a.Size); err != nil {
			return err
		}
		m, err := lookup(a.MapID)
		if err == nil {
			m.Artifacts = append(m.Artifacts, a)
		}
		return err
	}); err != nil {
		return err
	}
	if err := read(`SELECT map_id,virtual_path,start_name,dictionary_present FROM map_bsps WHERE map_id IN (SELECT value FROM json_each(?1)) ORDER BY map_id,virtual_path`, func(rows *sql.Rows) error {
		var id string
		var a domain.MapBSP
		if err := rows.Scan(&id, &a.VirtualPath, &a.StartName, &a.DictionaryPresent); err != nil {
			return err
		}
		m, err := lookup(id)
		if err == nil {
			m.BSPs = append(m.BSPs, a)
		}
		return err
	}); err != nil {
		return err
	}
	modeIndexes := make(map[string]map[string]int)
	if err := read(`SELECT mode.map_id,mode.name,chapter.ordinal,chapter.bsp_name,chapter.display_name,chapter.image FROM map_modes mode JOIN map_chapters chapter ON chapter.map_id=mode.map_id AND chapter.mode=mode.name WHERE mode.map_id IN (SELECT value FROM json_each(?1)) ORDER BY mode.map_id,mode.ordinal,chapter.ordinal`, func(rows *sql.Rows) error {
		var id, name string
		var a domain.MapChapter
		if err := rows.Scan(&id, &name, &a.Ordinal, &a.BSPName, &a.DisplayName, &a.Image); err != nil {
			return err
		}
		m, err := lookup(id)
		if err != nil {
			return err
		}
		if modeIndexes[id] == nil {
			modeIndexes[id] = make(map[string]int)
		}
		index, ok := modeIndexes[id][name]
		if !ok {
			index = len(m.Modes)
			modeIndexes[id][name] = index
			m.Modes = append(m.Modes, domain.MapMode{Name: name})
		}
		m.Modes[index].Chapters = append(m.Modes[index].Chapters, a)
		return nil
	}); err != nil {
		return err
	}
	if err := read(`SELECT map_id,code,message,virtual_path FROM map_warnings WHERE map_id IN (SELECT value FROM json_each(?1)) ORDER BY map_id,id`, func(rows *sql.Rows) error {
		var id string
		var a domain.MapWarning
		if err := rows.Scan(&id, &a.Code, &a.Message, &a.VirtualPath); err != nil {
			return err
		}
		m, err := lookup(id)
		if err == nil {
			m.Warnings = append(m.Warnings, a)
		}
		return err
	}); err != nil {
		return err
	}
	if err := read(`SELECT map_id,filesystem_id,health,verified_at FROM map_replicas WHERE map_id IN (SELECT value FROM json_each(?1)) ORDER BY map_id,filesystem_id`, func(rows *sql.Rows) error {
		var id string
		var a domain.MapReplica
		if err := rows.Scan(&id, &a.FilesystemID, &a.Health, &a.VerifiedAt); err != nil {
			return err
		}
		m, err := lookup(id)
		if err == nil {
			m.Replicas = append(m.Replicas, a)
		}
		return err
	}); err != nil {
		return err
	}
	return read(`SELECT map_id,instance_id,restart_required,created_at,updated_at FROM map_assignments WHERE map_id IN (SELECT value FROM json_each(?1)) ORDER BY map_id,instance_id`, func(rows *sql.Rows) error {
		var a domain.MapAssignment
		if err := rows.Scan(&a.MapID, &a.InstanceID, &a.RestartRequired, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return err
		}
		m, err := lookup(a.MapID)
		if err == nil {
			m.Assignments = append(m.Assignments, a)
		}
		return err
	})
}
