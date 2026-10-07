package importapply

import (
	"context"
	"encoding/json"
	"fmt"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
)

func planRecord(id, parent string, row int) p.Record {
	return p.Record{Ordinal: row, Values: map[p.Field]p.Value{"id": {Valid: true, Text: id}, "parent_id": {Valid: true, Text: parent, IsNull: parent == ""}}}
}
func decision(e p.Entity, row int, kind c.DecisionKind) c.RowDecision {
	return c.RowDecision{Ref: c.RowRef{Entity: e, Row: row}, Kind: kind}
}
func TestAppendProtectedEntities(t *testing.T) {
	d := p.Document{Tables: map[p.Entity][]p.Record{}}
	ds := []c.RowDecision{}
	for _, e := range []p.Entity{"tenants", "external_identities", "admin_grants", "users"} {
		d.Tables[e] = []p.Record{planRecord(string(e), "", 1)}
		ds = append(ds, decision(e, 1, c.New))
	}
	plan, err := BuildPlan(context.Background(), d, ds, c.NewCollector())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Counts["total"].Conflict != 3 || plan.Counts["total"].New != 1 || len(plan.Rows) != 1 || plan.Rows[0].Ref.Entity != "users" {
		t.Fatal("protected insert selected")
	}
	if plan.ErrorsTotal != 3 || len(plan.Issues) != 3 {
		t.Fatal("protected errors missing")
	}
	for _, i := range plan.Issues {
		if i.Code != "PROTECTED_ENTITY_NEW" {
			t.Fatal("wrong reason")
		}
	}
	if _, err = json.Marshal(plan); err == nil {
		t.Fatal("plan leaked document")
	}
	if _, err = json.Marshal(plan.Rows[0]); err == nil {
		t.Fatal("row leaked values")
	}
}
func TestAppendPlanStableOrder(t *testing.T) {
	d := p.Document{Tables: map[p.Entity][]p.Record{
		"organizations": {planRecord("child", "parent", 1), planRecord("parent", "", 2)},
		"departments":   {planRecord("depchild", "dep", 1), planRecord("dep", "", 2)},
		"users":         {planRecord("user", "", 1)}, "legal_entities": {planRecord("legal", "", 1)},
		"user_organizations": {planRecord("member", "", 1)}, "user_departments": {planRecord("deptmember", "", 1)},
	}}
	ds := []c.RowDecision{}
	for e, rs := range d.Tables {
		for _, r := range rs {
			ds = append(ds, decision(e, r.Ordinal, c.New))
		}
	}
	plan, err := BuildPlan(context.Background(), d, ds, c.NewCollector())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"legal", "parent", "child", "dep", "depchild", "user", "member", "deptmember"}
	if len(plan.Rows) != len(want) {
		t.Fatal("wrong insert count")
	}
	for i, r := range plan.Rows {
		if r.Record.Values["id"].Text != want[i] {
			t.Fatalf("row %d wrong order", i)
		}
	}
	// Identical parents need no second insert.
	ds = []c.RowDecision{decision("organizations", 1, c.New), decision("organizations", 2, c.Identical)}
	d = p.Document{Tables: map[p.Entity][]p.Record{"organizations": d.Tables["organizations"]}}
	plan, err = BuildPlan(context.Background(), d, ds, c.NewCollector())
	if err != nil || len(plan.Rows) != 1 || plan.Rows[0].Ref.Row != 1 {
		t.Fatal("existing parent duplicated")
	}
}
func TestAppendPlanDeepTree(t *testing.T) {
	d := p.Document{Tables: map[p.Entity][]p.Record{"organizations": {}}}
	ds := []c.RowDecision{}
	for n := 10000; n >= 1; n-- {
		parent := ""
		if n > 1 {
			parent = fmt.Sprintf("%05d", n-1)
		}
		row := 10001 - n
		d.Tables["organizations"] = append(d.Tables["organizations"], planRecord(fmt.Sprintf("%05d", n), parent, row))
		ds = append(ds, decision("organizations", row, c.New))
	}
	plan, err := BuildPlan(context.Background(), d, ds, c.NewCollector())
	if err != nil || len(plan.Rows) != 10000 {
		t.Fatalf("deep tree: %v", err)
	}
	for i, r := range plan.Rows {
		if r.Record.Values["id"].Text != fmt.Sprintf("%05d", i+1) {
			t.Fatal("chain order wrong")
		}
	}
}
func TestAppendDecisionTruncation(t *testing.T) {
	d := p.Document{Tables: map[p.Entity][]p.Record{"users": {}}}
	ds := []c.RowDecision{}
	issues := c.NewCollector()
	for n := 1; n <= 201; n++ {
		d.Tables["users"] = append(d.Tables["users"], planRecord(fmt.Sprint(n), "", n))
		ds = append(ds, decision("users", n, c.Conflict))
		issues.Add(c.Issue{Stage: "database", Issue: p.Issue{Entity: "users", Row: n, Field: "id", Code: "GLOBAL_KEY_CONFLICT"}})
	}
	plan, err := BuildPlan(context.Background(), d, ds, issues)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Counts["total"].Conflict != 201 || len(plan.Rows) != 0 || plan.ErrorsTotal != 201 || len(plan.Issues) != 200 || !plan.IssuesTruncated {
		t.Fatal("truncated issue became insert")
	}
}
func TestAppendPlanRejectsPartialDecisions(t *testing.T) {
	d := p.Document{Tables: map[p.Entity][]p.Record{"users": {planRecord("u", "", 1)}}}
	if _, err := BuildPlan(context.Background(), d, nil, c.NewCollector()); err == nil {
		t.Fatal("missing decision silently inserted")
	}
}
