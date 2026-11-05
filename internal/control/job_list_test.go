package control

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestJobPagesFindOldActiveJobsAndScopeTotals(t *testing.T) {
	s := packageRenameStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('other','Other','online');
	INSERT INTO jobs(id,node_id,type,status,created_at) VALUES('old-active','node','restart','running',?1),('private','other','restart','failed',?1);
	WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<120)
	INSERT INTO jobs(id,node_id,type,status,created_at) SELECT printf('done-%03d',x),'node','restart','succeeded',?2 FROM n`, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	scope := &jobVisibilityScope{NodeIDs: []string{"node"}}
	items, _, summary, err := s.ListJobPage(ctx, "", "", jobBackgroundExclude, scope, jobListFilter{Status: "active"}, 100)
	if err != nil || len(items) != 1 || items[0].ID != "old-active" || summary.Active != 1 || summary.Succeeded != 120 || summary.Failed != 0 {
		t.Fatalf("active/summary: %v %#v %#v", err, items, summary)
	}
	seen := map[string]bool{}
	f := jobListFilter{}
	for {
		items, next, _, err := s.ListJobPage(ctx, "", "", jobBackgroundExclude, scope, f, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if seen[item.ID] || item.ID == "private" {
				t.Fatalf("duplicate or unauthorized: %s", item.ID)
			}
			seen[item.ID] = true
		}
		if next == "" {
			break
		}
		f, err = parseJobListFilter(url.Values{"cursor": {next}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 121 {
		t.Fatalf("pagination missed jobs: %d", len(seen))
	}
	items, _, summary, err = s.ListJobPage(ctx, "", "", jobBackgroundExclude, scope, jobListFilter{Search: "old-active"}, 100)
	if err != nil || len(items) != 1 || summary.Active != 1 || summary.Succeeded != 0 {
		t.Fatalf("search: %v %#v %#v", err, items, summary)
	}
	items, _, summary, err = s.ListJobPage(ctx, "", "", jobBackgroundExclude, &jobVisibilityScope{}, jobListFilter{}, 100)
	if err != nil || len(items) != 0 || summary.Active != 0 || summary.Succeeded != 0 {
		t.Fatal("empty scope exposed jobs")
	}
}

func TestJobPageRejectsInvalidFilters(t *testing.T) {
	for _, v := range []url.Values{{"status": {"unknown"}}, {"cursor": {"not-a-cursor"}}, {"q": {string(make([]byte, 201))}}} {
		if _, err := parseJobListFilter(v); err == nil {
			t.Fatalf("accepted %#v", v)
		}
	}
}
