package importcompare

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
)

func TestCompareUnitAdjacentAndPrimary(t *testing.T) {
	for _, primary := range []bool{false, true} {
		in := singleDocument(t)
		stored := cloneDoc(in)
		stored.Tables["user_organizations"] = stored.Tables["user_organizations"][:1]
		stored.Tables["user_departments"] = nil
		stored.Tables["admin_grants"] = nil
		in.Tables["user_departments"] = nil
		in.Tables["admin_grants"] = nil
		r := cloneRecord(stored.Tables["user_organizations"][0])
		text(&r, "id", newID(501))
		stamp(&stored.Tables["user_organizations"][0], "effective_from", "2026-01-01T00:00:00Z")
		stamp(&stored.Tables["user_organizations"][0], "effective_to", "2026-03-01T00:00:00Z")
		stamp(&r, "effective_from", "2026-03-01T08:00:00+08:00")
		stamp(&r, "effective_to", "2026-04-01T00:00:00Z")
		r.Values["is_primary"] = p.Value{Valid: true, Bool: primary}
		if primary {
			stamp(&r, "effective_from", "2026-02-01T00:00:00Z")
			for _, org := range in.Tables["organizations"] {
				if org.Values["org_type"].Text != "virtual_group" && org.Values["id"].Text != r.Values["organization_id"].Text {
					text(&r, "organization_id", org.Values["id"].Text)
					break
				}
			}
		}
		in.Tables["user_organizations"] = []p.Record{r}
		renumber(&in)
		renumber(&stored)
		cl, is, err := Compare(context.Background(), in, snapshot(stored))
		if err != nil {
			t.Fatal(err)
		}
		if primary {
			if *cl["user_organizations"].Conflict != 1 || !found(is, "PRIMARY_OVERLAP", "user_organizations", 1) {
				t.Fatal("primary across organizations")
			}
		} else if *cl["user_organizations"].New != 1 || is.Total() != 0 {
			t.Fatal("half-open microsecond adjacency")
		}
	}
}
func TestCompareUnitDepartmentContains(t *testing.T) {
	in := singleDocument(t)
	stored := cloneDoc(in)
	stored.Tables["user_departments"] = nil
	stored.Tables["admin_grants"] = nil
	in.Tables["admin_grants"] = nil
	r := cloneRecord(in.Tables["user_departments"][0])
	parent := r.Values["user_organization_id"].Text
	for n := range in.Tables["user_organizations"] {
		if in.Tables["user_organizations"][n].Values["id"].Text == parent {
			stamp(&in.Tables["user_organizations"][n], "effective_from", "2026-01-01T00:00:00Z")
			stamp(&in.Tables["user_organizations"][n], "effective_to", "2026-04-01T00:00:00Z")
			stored.Tables["user_organizations"] = []p.Record{cloneRecord(in.Tables["user_organizations"][n])}
			stamp(&stored.Tables["user_organizations"][0], "effective_to", "2026-02-01T00:00:00Z")
			in.Tables["user_organizations"] = []p.Record{in.Tables["user_organizations"][n]}
			break
		}
	}
	text(&r, "id", newID(502))
	stamp(&r, "effective_from", "2026-03-01T00:00:00Z")
	stamp(&r, "effective_to", "2026-04-01T00:00:00Z")
	in.Tables["user_departments"] = []p.Record{r}
	renumber(&in)
	renumber(&stored)
	cl, is, err := Compare(context.Background(), in, snapshot(stored))
	if err != nil || *cl["user_departments"].Conflict != 1 || !found(is, "DEPARTMENT_INTERVAL_OUTSIDE", "user_departments", 1) {
		t.Fatal("proposed parent change was incorrectly applied")
	}
}
func TestCompareUnitStoredTrees(t *testing.T) {
	for _, entity := range []p.Entity{"organizations", "departments"} {
		in := singleDocument(t)
		stored := cloneDoc(in)
		r := &stored.Tables[entity][0]
		text(r, "parent_id", r.Values["id"].Text)
		cl, _, err := Compare(context.Background(), in, snapshot(stored))
		if err == nil || err.Error() != "DATABASE_DATA_INVALID" || cl != nil {
			t.Fatal("stored tree invalid was treated as compatible")
		}
	}
}
