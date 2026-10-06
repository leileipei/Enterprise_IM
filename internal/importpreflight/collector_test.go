package importpreflight

import (
	"bytes"
	"testing"
)

func TestCollectorSortedDedupAndTruncation(t *testing.T) {
	c := NewCollector()
	for i := 251; i > 0; i-- {
		v := Issue{Entity: "users", Row: i, Field: "id", Code: "PK_DUPLICATE"}
		c.Add(v)
		c.Add(v)
	}
	r := BuildReport(Outcome{Status: "invalid", ChecksComplete: true}, c)
	if r.ErrorsTotal != 251 || len(r.Issues) != 200 || !r.IssuesTruncated {
		t.Fatal("incorrect total or truncation")
	}
	for i, v := range r.Issues {
		if v.Row != i+1 {
			t.Fatalf("position %d: row %d", i, v.Row)
		}
	}
	d := NewCollector()
	for i := 1; i <= 251; i++ {
		d.Add(Issue{Entity: "users", Row: i, Field: "id", Code: "PK_DUPLICATE"})
	}
	a, _ := EncodeReport(r)
	b, _ := EncodeReport(BuildReport(Outcome{Status: "invalid", ChecksComplete: true}, d))
	if !bytes.Equal(a, b) {
		t.Fatal("nondeterministic order")
	}
	e := NewCollector()
	for _, v := range []Issue{{"users", 1, "_unknown", "TYPE_INVALID", 0}, {"users", 1, "tenant_id", "REF_NOT_FOUND", 0}, {"users", 1, "id", "PK_DUPLICATE", 2}, {"users", 1, "id", "PK_DUPLICATE", 0}, {"document", 0, "_document", "JSON_INVALID", 0}} {
		e.Add(v)
	}
	q := BuildReport(Outcome{Status: "invalid"}, e)
	if q.Issues[0].Entity != "document" || q.Issues[1].RelatedRow != 0 || q.Issues[2].RelatedRow != 2 || q.Issues[3].Field != "tenant_id" {
		t.Fatal("canonical ordering")
	}
}
