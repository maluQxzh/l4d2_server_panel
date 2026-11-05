package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMetricPaginationPreservesEntireWindowAndTimestampTies(t *testing.T) {
	for _, resolution := range []string{"raw", "1m", "1h"} {
		t.Run(resolution, func(t *testing.T) {
			s := packageRenameStore(t)
			ctx := context.Background()
			from := time.Now().UTC().Truncate(time.Hour).Add(-7 * 24 * time.Hour)
			query := metricQuery{NodeID: "node", Resolution: resolution, From: from, To: from.Add(7 * 24 * time.Hour)}
			statement := `WITH RECURSIVE n(x) AS (SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x<10080)
			INSERT INTO metrics_1m(node_id,instance_id,bucket,players,sample_count) SELECT 'node','instance',?1+x*60000000,1,1 FROM n`
			if resolution == "1h" {
				statement = strings.ReplaceAll(statement, "metrics_1m", "metrics_1h")
			}
			if resolution == "raw" {
				statement = `WITH RECURSIVE n(x) AS (SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x<10080)
				INSERT INTO metrics(node_id,instance_id,collected_at,players) SELECT 'node','instance',?1,1 FROM n`
			}
			if _, err := s.db.ExecContext(ctx, statement, from.UnixMicro()); err != nil {
				t.Fatal(err)
			}
			items, next, err := s.Metrics(ctx, query)
			if err != nil || len(items) != 10000 || next == "" {
				t.Fatalf("first page: count=%d next=%q err=%v", len(items), next, err)
			}
			var cursor metricCursor
			data, _ := base64.RawURLEncoding.DecodeString(next)
			if err := json.Unmarshal(data, &cursor); err != nil {
				t.Fatal(err)
			}
			query.Cursor = &cursor
			tail, next, err := s.Metrics(ctx, query)
			if err != nil || len(tail) != 81 || next != "" {
				t.Fatalf("tail: count=%d next=%q err=%v", len(tail), next, err)
			}
			if resolution != "raw" && !tail[len(tail)-1].CollectedAt.Equal(query.To) {
				t.Fatal("latest 81 minutes were lost")
			}
		})
	}
}

func TestMetricCursorBindsScopeAndWindow(t *testing.T) {
	now := time.Now().UTC()
	id := "d0f2bc53-b66d-4e3b-9f98-c04de97be58f"
	c := metricCursor{At: now.Add(-time.Minute), From: now.Add(-time.Hour), To: now, ScopeNode: id, Resolution: "1m", Node: id}
	raw, _ := json.Marshal(c)
	values := url.Values{"node_id": {id}, "cursor": {base64.RawURLEncoding.EncodeToString(raw)}}
	q, err := parseMetricQuery(values, now.Add(time.Hour))
	if err != nil || !q.To.Equal(now) {
		t.Fatalf("range not pinned: %#v %v", q, err)
	}
	values.Set("resolution", "1h")
	if _, err := parseMetricQuery(values, now); err == nil {
		t.Fatal("accepted changed resolution")
	}
	values.Set("resolution", "1m")
	values.Set("to", now.Add(time.Minute).Format(time.RFC3339))
	if _, err := parseMetricQuery(values, now); err == nil {
		t.Fatal("accepted changed range")
	}
}

func TestMetricTimeIndexesServeQueriesAndRetention(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	from := time.Now().UTC().Truncate(time.Minute).Add(-30 * 24 * time.Hour)
	// A full month for ten nodes, including the instance queried below.
	if _, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<9)
	INSERT INTO nodes(id,name,status) SELECT 'node-'||x,'Node','online' FROM n;
	WITH RECURSIVE n(x) AS (SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x<43200)
	INSERT INTO metrics_1m(node_id,instance_id,bucket,sample_count)
	SELECT nodes.id,CASE WHEN nodes.id='node' THEN 'instance' ELSE NULL END,?1+n.x*60000000,1 FROM nodes,n`, from.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	for _, resolution := range []string{"raw", "1m", "1h"} {
		q := metricQuery{InstanceID: "instance", Resolution: resolution, From: from.Add(29 * 24 * time.Hour), To: from.Add(30 * 24 * time.Hour)}
		statement, args := metricStatement(q)
		assertQueryUsesIndex(t, s, "EXPLAIN QUERY PLAN "+statement, args, "instance_time_idx")
	}
	for _, tc := range []struct{ table, column string }{{"metrics", "collected_at"}, {"metrics_1m", "bucket"}, {"metrics_1h", "bucket"}} {
		assertQueryUsesIndex(t, s, `EXPLAIN QUERY PLAN SELECT max(`+tc.column+`) FROM `+tc.table, nil, tc.table+"_time_idx")
		assertQueryUsesIndex(t, s, `EXPLAIN QUERY PLAN DELETE FROM `+tc.table+` WHERE `+tc.column+`<?1`, []any{from}, tc.table+"_time_idx")
		assertQueryUsesIndex(t, s, `EXPLAIN QUERY PLAN SELECT node_id,instance_id,avg(cpu_percent) FROM `+tc.table+` INDEXED BY `+tc.table+`_time_idx WHERE `+tc.column+`>=?1 GROUP BY node_id,instance_id`, []any{from.Add(29 * 24 * time.Hour)}, tc.table+"_time_idx")
	}
	started := time.Now()
	items, next, err := s.Metrics(ctx, metricQuery{InstanceID: "instance", Resolution: "1m", From: from.Add(29 * 24 * time.Hour), To: from.Add(30 * 24 * time.Hour)})
	if err != nil || len(items) != 1441 || next != "" {
		t.Fatalf("month dataset query: %d %v", len(items), err)
	}
	t.Logf("432010 stored samples; 1441-point instance query: %s", time.Since(started))
}

func assertQueryUsesIndex(t *testing.T, s *Store, query string, args []any, index string) {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), index) {
		t.Fatalf("missing %s:\n%s", index, plan.String())
	}
}
