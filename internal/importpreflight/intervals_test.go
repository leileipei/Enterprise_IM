package importpreflight

import (
	"context"
	"testing"
)

func timed(s string) Value {
	tm, ok := parseTimestamp(s)
	if !ok {
		panic("invalid test time")
	}
	return Value{Valid: true, Time: &tm}
}
func checkIntervals(t *testing.T, d Document) Report {
	t.Helper()
	c := NewCollector()
	idx, e := BuildIndex(context.Background(), d, c)
	if e != nil {
		t.Fatal(e)
	}
	rels, e := CheckReferences(context.Background(), d, idx, c)
	if e != nil {
		t.Fatal(e)
	}
	if e = CheckIntervals(context.Background(), d, idx, rels, c); e != nil {
		t.Fatal(e)
	}
	return BuildReport(Outcome{Status: "invalid", ChecksComplete: true}, c)
}
func TestIntervalsHistoricalAndAdjacent(t *testing.T) {
	for _, status := range []string{"ended", "suspended"} {
		d := sampleDocument(t)
		r := duplicate(&d, "user_organizations", 2)
		setText(&r, "id", "00000000-0000-0000-0000-000000000123")
		setText(&r, "status", status)
		d.Tables["user_organizations"] = append(d.Tables["user_organizations"], r)
		rep := checkIntervals(t, d)
		if !hasCode(rep, "INTERVAL_OVERLAP") || !hasCode(rep, "PRIMARY_OVERLAP") {
			t.Fatal("historical exclusion skipped")
		}
	}
	d := sampleDocument(t)
	d.Tables["user_departments"] = nil
	d.Tables["admin_grants"] = nil
	d.Tables["user_organizations"] = d.Tables["user_organizations"][:1]
	d.Tables["user_organizations"][0].Values["effective_to"] = timed("2026-10-01T00:00:00.000001+08:00")
	r := duplicate(&d, "user_organizations", 0)
	setText(&r, "id", "00000000-0000-0000-0000-000000000124")
	r.Values["effective_from"] = timed("2026-09-30T16:00:00.000001Z")
	r.Values["effective_to"] = Value{Valid: true, IsNull: true}
	d.Tables["user_organizations"] = append(d.Tables["user_organizations"], r)
	rep := checkIntervals(t, d)
	if rep.ErrorsTotal != 0 {
		t.Fatalf("adjacent %+v", rep.Issues)
	}
	d.Tables["user_organizations"][1].Values["effective_from"] = timed("2026-09-30T16:00:00Z")
	if !hasCode(checkIntervals(t, d), "INTERVAL_OVERLAP") {
		t.Fatal("microsecond overlap missed")
	}
	d.Tables["user_organizations"][0].Values["effective_to"] = d.Tables["user_organizations"][0].Values["effective_from"]
	if !hasCode(checkIntervals(t, d), "INTERVAL_INVALID") {
		t.Fatal("empty interval accepted")
	}
}
func TestDepartmentIntervalsAndPrimary(t *testing.T) {
	d := sampleDocument(t)
	r := duplicate(&d, "user_departments", 1)
	setText(&r, "id", "00000000-0000-0000-0000-000000000125")
	r.Values["department_id"] = d.Tables["departments"][1].Values["id"]
	d.Tables["user_departments"] = append(d.Tables["user_departments"], r)
	if !hasCode(checkIntervals(t, d), "PRIMARY_OVERLAP") {
		t.Fatal("department primary overlap")
	}
	d = sampleDocument(t)
	d.Tables["user_organizations"][2].Values["effective_to"] = timed("2026-10-01T00:00:00Z")
	if !hasCode(checkIntervals(t, d), "DEPARTMENT_INTERVAL_OUTSIDE") {
		t.Fatal("parent shrink")
	}
	setText(&d.Tables["user_departments"][2], "user_organization_id", "00000000-0000-0000-0000-000000000999")
	r2 := checkIntervals(t, d)
	if !hasCode(r2, "REF_NOT_FOUND") || hasCode(r2, "DEPARTMENT_INTERVAL_OUTSIDE") {
		t.Fatal("missing parent causes derived interval")
	}
}
func TestGrantScopeAndHistoricalData(t *testing.T) {
	d := sampleDocument(t)
	d.Tables["admin_grants"][0].Values["scope_organization_id"] = d.Tables["organizations"][1].Values["id"]
	if !hasCode(checkIntervals(t, d), "GRANT_SCOPE_INVALID") {
		t.Fatal("group scope")
	}
	d = sampleDocument(t)
	d.Tables["admin_grants"][1].Values["scope_organization_id"] = Value{Valid: true, IsNull: true}
	if !hasCode(checkIntervals(t, d), "GRANT_SCOPE_INVALID") {
		t.Fatal("org null scope")
	}
	d = sampleDocument(t)
	d.Tables["user_departments"] = nil
	d.Tables["user_organizations"][0].Values["effective_to"] = timed("2026-02-01T00:00:00Z")
	setText(&d.Tables["user_organizations"][0], "status", "ended")
	d.Tables["admin_grants"][0].Values["effective_from"] = timed("2026-03-01T00:00:00Z")
	setText(&d.Tables["admin_grants"][0], "status", "revoked")
	for i := range d.Tables["organizations"] {
		setText(&d.Tables["organizations"][i], "status", "disabled")
	}
	if r := checkIntervals(t, d); r.ErrorsTotal != 0 {
		t.Fatalf("unrequested history constraints %+v", r.Issues)
	}
}
