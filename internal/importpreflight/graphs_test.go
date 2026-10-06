package importpreflight

import (
	"context"
	"fmt"
	"testing"
)

func TestGraphsCyclesAndCrossLegalDivision(t *testing.T) {
	d := sampleDocument(t)
	_, _, rep := checkModel(t, d)
	if rep.ErrorsTotal != 0 {
		t.Fatalf("legal cross-legal division: %+v", rep.Issues)
	}
	for _, e := range []Entity{"organizations", "departments"} {
		d = sampleDocument(t)
		i := 0
		if e == "departments" {
			i = 1
		}
		d.Tables[e][i].Values["parent_id"] = d.Tables[e][i].Values["id"]
		_, _, rep = checkModel(t, d)
		if !hasCode(rep, "SELF_PARENT") || hasCode(rep, "TREE_CYCLE") {
			t.Fatal("self cycle classification")
		}
	}
	d = sampleDocument(t)
	for i, j := range []int{1, 2, 0} {
		d.Tables["organizations"][i].Values["parent_id"] = d.Tables["organizations"][j].Values["id"]
	}
	d.Tables["organizations"][3].Values["parent_id"] = d.Tables["organizations"][0].Values["id"]
	_, _, rep = checkModel(t, d)
	n := 0
	for _, i := range rep.Issues {
		if i.Code == "TREE_CYCLE" {
			n++
			if i.Row > 3 {
				t.Fatal("prefix marked cycle")
			}
		}
	}
	if n != 3 {
		t.Fatalf("cycle nodes %d", n)
	}
	d = sampleDocument(t)
	d.Tables["departments"][1].Values["parent_id"] = d.Tables["departments"][2].Values["id"]
	d.Tables["departments"][2].Values["parent_id"] = d.Tables["departments"][1].Values["id"]
	_, _, rep = checkModel(t, d)
	n = 0
	for _, i := range rep.Issues {
		if i.Code == "TREE_CYCLE" {
			n++
		}
	}
	if n != 2 {
		t.Fatal("department cycle")
	}
}
func TestDeepGraphBudgetAndCancellation(t *testing.T) {
	d := sampleDocument(t)
	d.Tables["organizations"] = nil
	proto := sampleDocument(t).Tables["organizations"][0]
	for i := 0; i < 9900; i++ {
		r := Record{Ordinal: i + 1, Values: map[Field]Value{}}
		for f, v := range proto.Values {
			r.Values[f] = v
		}
		setText(&r, "id", fmt.Sprintf("00000000-0000-0000-0002-%012x", i))
		setText(&r, "code", fmt.Sprint(i))
		if i > 0 {
			r.Values["parent_id"] = d.Tables["organizations"][i-1].Values["id"]
		}
		d.Tables["organizations"] = append(d.Tables["organizations"], r)
	}
	for _, e := range []Entity{"departments", "user_organizations", "user_departments", "admin_grants"} {
		d.Tables[e] = nil
	}
	_, _, r := checkModel(t, d)
	if r.ErrorsTotal != 0 {
		t.Fatalf("deep chain rejected %+v", r.Issues)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewCollector()
	idx, e := BuildIndex(ctx, d, c)
	if e == nil {
		t.Fatal("index cancellation")
	}
	idx, e = BuildIndex(context.Background(), d, c)
	if e != nil {
		t.Fatal(e)
	}
	rels, e := CheckReferences(ctx, d, idx, c)
	if e == nil {
		t.Fatal("references cancellation")
	}
	if CheckGraphs(ctx, d, idx, rels, c) == nil {
		t.Fatal("graphs cancellation")
	}
}
