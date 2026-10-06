package importpreflight

import (
	"context"
	"testing"
)

func setText(r *Record, f Field, s string) { r.Values[f] = Value{Valid: true, Text: s} }
func checkModel(t *testing.T, d Document) (*Index, Relations, Report) {
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
	if e = CheckGraphs(context.Background(), d, idx, rels, c); e != nil {
		t.Fatal(e)
	}
	return idx, rels, BuildReport(Outcome{Status: "invalid", ChecksComplete: true}, c)
}
func duplicate(d *Document, e Entity, i int) Record {
	old := d.Tables[e][i]
	r := Record{Ordinal: len(d.Tables[e]) + 1, Values: map[Field]Value{}}
	for f, v := range old.Values {
		r.Values[f] = v
	}
	return r
}
func TestIndexCompositeUniqueness(t *testing.T) {
	d := sampleDocument(t)
	r := duplicate(&d, "users", 0)
	setText(&r, "tenant_id", d.Tables["tenants"][1].Values["id"].Text)
	d.Tables["users"] = append(d.Tables["users"], r)
	idx, _, rep := checkModel(t, d)
	if _, ok := idx.Find("users", r.Values["id"].Text); ok || !hasCode(rep, "PK_DUPLICATE") {
		t.Fatal("ambiguous id used")
	}
	for _, mode := range []string{"pk", "unique", "employee"} {
		d = sampleDocument(t)
		e := Entity("external_identities")
		r = duplicate(&d, e, 0)
		if mode == "unique" {
			setText(&r, "subject", "different")
		}
		if mode == "employee" {
			e = "users"
			r = duplicate(&d, e, 0)
			setText(&r, "id", "00000000-0000-0000-0000-000000000123")
		}
		d.Tables[e] = append(d.Tables[e], r)
		_, _, rep = checkModel(t, d)
		code := Code("UNIQUE_DUPLICATE")
		if mode == "pk" {
			code = "PK_DUPLICATE"
		}
		if !hasCode(rep, code) {
			t.Fatalf("%s %+v", mode, rep.Issues)
		}
	}
	d = sampleDocument(t)
	d.Tables["departments"][2].Values["code"] = d.Tables["departments"][0].Values["code"]
	_, _, rep = checkModel(t, d)
	if hasCode(rep, "UNIQUE_DUPLICATE") {
		t.Fatal("different organizations share code legally")
	}
	b := mutateBytes(t, sampleBytes(t), func(m map[string]any) {
		rows := tableRows(m, "users")
		x := map[string]any{}
		for k, v := range rows[0].(map[string]any) {
			x[k] = v
		}
		x["id"] = "AE5368D5-748F-56C0-A4F8-1D486BA4E6CA"
		rows[0].(map[string]any)["id"] = "ae5368d5-748f-56c0-a4f8-1d486ba4e6ca"
		m["tables"].(map[string]any)["users"] = append(rows, x)
	})
	doc, _, _, _ := normalizeBytes(t, b)
	_, _, rep = checkModel(t, doc)
	if !hasCode(rep, "PK_DUPLICATE") {
		t.Fatal("uppercase id differs")
	}
}
