package importcompare

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
	"time"
)

func TestCompareUnitClassify(t *testing.T) {
	d := singleDocument(t)
	s := snapshot(cloneDoc(d))
	c, issues, err := Compare(context.Background(), d, s)
	if err != nil || *c["total"].Identical != 66 || *c["total"].Conflict != 0 || issues.Total() != 0 {
		t.Fatal("identical file")
	}
	u := cloneRecord(d.Tables["users"][0])
	text(&u, "id", newID(1))
	text(&u, "global_employee_no", "synthetic-new-person")
	u.Ordinal = 12
	d.Tables["users"] = append(d.Tables["users"], u)
	c, _, err = Compare(context.Background(), d, s)
	if err != nil || *c["users"].New != 1 || *c["total"].Identical != 66 {
		t.Fatal("new row")
	}
	for _, schema := range p.Schema() {
		for _, field := range schema.Fields {
			if field.Name == "id" || schema.Entity == "external_identities" && (field.Name == "issuer" || field.Name == "subject") {
				continue
			}
			t.Run(string(schema.Entity)+"/"+string(field.Name), func(t *testing.T) {
				in := cloneDoc(s.Data)
				r := &in.Tables[schema.Entity][0]
				v := r.Values[field.Name]
				switch field.Kind {
				case "bool":
					v.Bool = !v.Bool
				case "time":
					tm := time.Date(2030, 1, 1, 0, 0, 0, 123456, time.UTC)
					if v.Time != nil {
						tm = v.Time.Add(time.Microsecond)
					}
					v = p.Value{Valid: true, Time: &tm}
				default:
					if v.IsNull {
						v = p.Value{Valid: true, Text: ""}
					} else {
						v.Text += "synthetic-difference"
					}
				}
				r.Values[field.Name] = v
				cl, is, e := Compare(context.Background(), in, s)
				if e != nil || *cl[string(schema.Entity)].Conflict < 1 || !found(is, "STORED_VALUE_DIFFERS", schema.Entity, 1) {
					t.Fatal("declared field difference missing")
				}
			})
		}
	}
}
func TestCompareUnitGlobalKeys(t *testing.T) {
	d := singleDocument(t)
	s := snapshot(cloneDoc(d))
	for _, e := range []p.Entity{"users", "external_identities"} {
		in := cloneDoc(d)
		r := cloneRecord(in.Tables[e][0])
		r.Ordinal = len(in.Tables[e]) + 1
		if e == "users" {
			text(&r, "id", newID(91))
			text(&r, "global_employee_no", "unique-global-key")
		} else {
			text(&r, "issuer", "new-issuer")
			text(&r, "subject", "new-subject")
		}
		in.Tables[e] = append(in.Tables[e], r)
		s.GlobalKeys = map[RowRef]bool{{Entity: e, Row: r.Ordinal}: true}
		cl, is, err := Compare(context.Background(), in, s)
		if err != nil || *cl[string(e)].Conflict != 1 || !found(is, "GLOBAL_KEY_CONFLICT", e, r.Ordinal) {
			t.Fatal("global occupied key")
		}
	}
}
func TestCompareUnitStoredUnique(t *testing.T) {
	d := singleDocument(t)
	for _, e := range []p.Entity{"legal_entities", "organizations", "departments", "users", "external_identities"} {
		in := cloneDoc(d)
		r := cloneRecord(in.Tables[e][0])
		r.Ordinal = len(in.Tables[e]) + 1
		if e == "external_identities" {
			text(&r, "subject", "new-subject-for-same-issuer")
		} else {
			text(&r, "id", newID(98))
		}
		in.Tables[e] = append(in.Tables[e], r)
		cl, is, err := Compare(context.Background(), in, snapshot(d))
		if err != nil || *cl[string(e)].Conflict < 1 || !found(is, "STORED_UNIQUE_CONFLICT", e, r.Ordinal) {
			t.Fatal("stored unique key")
		}
	}
}
func TestCompareUnitReverseInterval(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, status := range []string{"ended", "suspended"} {
			in := singleDocument(t)
			stored := cloneDoc(in)
			in.Tables["user_departments"] = nil
			in.Tables["admin_grants"] = nil
			candidate := cloneRecord(in.Tables["user_organizations"][0])
			text(&candidate, "id", newID(200))
			text(&candidate, "status", status)
			candidate.Values["is_primary"] = p.Value{Valid: true}
			a, b := "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"
			if reverse {
				a, b = b, a
			}
			stamp(&candidate, "effective_from", a)
			stamp(&candidate, "effective_to", "2026-03-01T00:00:00Z")
			in.Tables["user_organizations"] = []p.Record{candidate}
			stamp(&stored.Tables["user_organizations"][0], "effective_from", b)
			stamp(&stored.Tables["user_organizations"][0], "effective_to", "2026-03-01T00:00:00Z")
			stored.Tables["user_departments"] = nil
			stored.Tables["admin_grants"] = nil
			stored.Tables["user_organizations"] = stored.Tables["user_organizations"][:1]
			renumber(&in)
			renumber(&stored)
			cl, is, err := Compare(context.Background(), in, snapshot(stored))
			if err != nil || *cl["user_organizations"].Conflict != 1 || !found(is, "INTERVAL_OVERLAP", "user_organizations", 1) {
				t.Fatal("reverse interval lost input")
			}
			for _, i := range is.Issues() {
				if i.Issue.RelatedRow != 0 {
					t.Fatal("stored internal row exposed")
				}
			}
		}
	}
}
func TestCompareUnitMoreThan200Rows(t *testing.T) {
	in := singleDocument(t)
	member := cloneRecord(in.Tables["user_organizations"][0])
	user := cloneRecord(in.Tables["users"][0])
	for _, e := range []p.Entity{"users", "user_organizations", "user_departments", "external_identities", "admin_grants"} {
		in.Tables[e] = nil
	}
	stored := cloneDoc(in)
	for n := 1; n <= 201; n++ {
		u := cloneRecord(user)
		text(&u, "id", newID(1000+n))
		text(&u, "global_employee_no", newID(2000+n))
		in.Tables["users"] = append(in.Tables["users"], u)
		stored.Tables["users"] = append(stored.Tables["users"], u)
		r := cloneRecord(member)
		text(&r, "user_id", newID(1000+n))
		r.Values["is_primary"] = p.Value{Valid: true}
		stamp(&r, "effective_from", "2026-01-01T00:00:00Z")
		stamp(&r, "effective_to", "2026-04-01T00:00:00Z")
		text(&r, "id", newID(3000+n))
		stored.Tables["user_organizations"] = append(stored.Tables["user_organizations"], r)
		r = cloneRecord(r)
		text(&r, "id", newID(4000+n))
		stamp(&r, "effective_from", "2026-02-01T00:00:00Z")
		in.Tables["user_organizations"] = append(in.Tables["user_organizations"], r)
	}
	renumber(&in)
	renumber(&stored)
	cl, is, err := Compare(context.Background(), in, snapshot(stored))
	if err != nil || *cl["total"].Conflict != 201 || *cl["user_organizations"].Conflict != 201 || len(is.Issues()) != 200 || is.Total() != 201 {
		t.Fatal("201 conflicts were truncated")
	}
}
func TestCompareUnitStoredInvalid(t *testing.T) {
	in := singleDocument(t)
	s := snapshot(cloneDoc(in))
	text(&s.Data.Tables["organizations"][0], "parent_id", s.Data.Tables["organizations"][0].Values["id"].Text)
	cl, _, err := Compare(context.Background(), in, s)
	if err == nil || err.Error() != "DATABASE_DATA_INVALID" || cl != nil {
		t.Fatal("invalid stored model")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cl, _, err = Compare(ctx, in, snapshot(in))
	if err == nil || err.Error() != "CANCELED" || cl != nil {
		t.Fatal("partial canceled classification")
	}
}
