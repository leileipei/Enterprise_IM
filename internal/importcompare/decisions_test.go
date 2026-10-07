package importcompare

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
)

func TestAppendDecisionParity(t *testing.T) {
	d := singleDocument(t)
	decisions, issues, err := Decide(context.Background(), d, snapshot(cloneDoc(d)))
	if err != nil || len(decisions) != 66 || issues.Total() != 0 {
		t.Fatalf("decision parity: %v", err)
	}
	for _, decision := range decisions {
		if decision.Kind != Identical {
			t.Fatal("identical row became new")
		}
	}
	old, _, err := Compare(context.Background(), d, snapshot(d))
	if err != nil || *old["total"].Identical != 66 || *old["total"].New != 0 || *old["total"].Conflict != 0 {
		t.Fatal("legacy totals changed")
	}
	d.Tables["users"][0].Values["display_name"] = p.Value{Valid: true, Text: "difference"}
	decisions, _, err = Decide(context.Background(), d, snapshot(singleDocument(t)))
	if err != nil {
		t.Fatal(err)
	}
	conflicts := 0
	for _, decision := range decisions {
		if decision.Kind == Conflict {
			conflicts++
			if decision.Ref != (RowRef{"users", 1}) {
				t.Fatal("wrong input row")
			}
		}
	}
	if conflicts != 1 {
		t.Fatal("field difference lost")
	}
}
