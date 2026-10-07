package importcompare

import (
	"context"
	"encoding/json"
	"fmt"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
	"testing"
	"time"
)

func singleDocument(t *testing.T) p.Document {
	t.Helper()
	b, e := os.ReadFile("../importpreflight/testdata/sample_data_v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(b, &raw)
	var tables map[string][]map[string]json.RawMessage
	json.Unmarshal(raw["tables"], &tables)
	var tenant string
	json.Unmarshal(tables["tenants"][0]["id"], &tenant)
	for name, rows := range tables {
		out := []map[string]json.RawMessage{}
		for _, row := range rows {
			var id string
			k := "tenant_id"
			if name == "tenants" {
				k = "id"
			}
			json.Unmarshal(row[k], &id)
			if id == tenant {
				out = append(out, row)
			}
		}
		tables[name] = out
	}
	raw["tables"], _ = json.Marshal(tables)
	b, _ = json.Marshal(raw)
	report, d := p.EvaluateDocument(context.Background(), b)
	if d == nil || *report.Counts["total"] != 66 {
		t.Fatal("closed single tenant fixture")
	}
	return *d
}
func cloneDoc(d p.Document) p.Document {
	out := d
	out.Tables = map[p.Entity][]p.Record{}
	for e, rows := range d.Tables {
		out.Tables[e] = []p.Record{}
		for _, r := range rows {
			out.Tables[e] = append(out.Tables[e], cloneRecord(r))
		}
	}
	return out
}
func cloneRecord(r p.Record) p.Record {
	out := r
	out.Values = map[p.Field]p.Value{}
	for k, v := range r.Values {
		out.Values[k] = v
	}
	return out
}
func text(r *p.Record, f p.Field, v string) { r.Values[f] = p.Value{Valid: true, Text: v} }
func stamp(r *p.Record, f p.Field, v string) {
	tm, e := time.Parse(time.RFC3339Nano, v)
	if e != nil {
		panic(e)
	}
	r.Values[f] = p.Value{Valid: true, Time: &tm}
}
func newID(n int) string { return fmt.Sprintf("77777777-0000-4000-8000-%012d", n) }
func snapshot(d p.Document) Snapshot {
	return Snapshot{TenantFound: true, Data: d, GlobalKeys: map[RowRef]bool{}}
}
func found(c *Collector, code p.Code, entity p.Entity, row int) bool {
	for _, i := range c.Issues() {
		if i.Issue.Code == code && i.Issue.Entity == entity && (row == 0 || row == i.Issue.Row) {
			return true
		}
	}
	return false
}
func renumber(d *p.Document) {
	for e, rows := range d.Tables {
		for i := range rows {
			rows[i].Ordinal = i + 1
		}
		d.Tables[e] = rows
	}
}
