package importapply

import (
	"container/heap"
	"context"
	"errors"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"sort"
)

type TableCounts struct {
	Input     int `json:"input"`
	New       int `json:"new"`
	Identical int `json:"identical"`
	Conflict  int `json:"conflict"`
	Inserted  int `json:"inserted"`
}
type Issue struct {
	Entity p.Entity `json:"entity"`
	Row    int      `json:"row"`
	Field  p.Field  `json:"field"`
	Code   string   `json:"code"`
}
type PlannedRow struct {
	Ref    c.RowRef
	Record p.Record
}
type Plan struct {
	Rows            []PlannedRow
	Counts          map[string]TableCounts
	Issues          []Issue
	ErrorsTotal     int
	IssuesTruncated bool
}

var errPlan = errors.New("INVALID_APPEND_PLAN")

func (Plan) MarshalJSON() ([]byte, error)       { return nil, errPlan }
func (PlannedRow) MarshalJSON() ([]byte, error) { return nil, errPlan }

func BuildPlan(ctx context.Context, input p.Document, decisions []c.RowDecision, issues *c.Collector) (Plan, error) {
	fail := func(err error) (Plan, error) { return Plan{}, err }
	out := Plan{Rows: []PlannedRow{}, Counts: map[string]TableCounts{}, Issues: []Issue{}}
	if issues != nil {
		out.ErrorsTotal = issues.Total()
		for _, i := range issues.Issues() {
			out.Issues = append(out.Issues, Issue{i.Issue.Entity, i.Issue.Row, i.Issue.Field, string(i.Issue.Code)})
		}
	}
	byRef := map[c.RowRef]c.DecisionKind{}
	for _, d := range decisions {
		if _, ok := byRef[d.Ref]; ok {
			return fail(errPlan)
		}
		if d.Kind != c.New && d.Kind != c.Identical && d.Kind != c.Conflict {
			return fail(errPlan)
		}
		byRef[d.Ref] = d.Kind
	}
	for _, schema := range p.Schema() {
		e := schema.Entity
		count := TableCounts{}
		newRows := []PlannedRow{}
		protected := e == "tenants" || e == "external_identities" || e == "admin_grants"
		for _, r := range input.Tables[e] {
			if err := p.ContextFailure(ctx); err != nil {
				return fail(err)
			}
			ref := c.RowRef{Entity: e, Row: r.Ordinal}
			kind, ok := byRef[ref]
			if !ok || r.Ordinal < 1 {
				return fail(errPlan)
			}
			delete(byRef, ref)
			count.Input++
			if protected && kind == c.New {
				kind = c.Conflict
				out.ErrorsTotal++
				if len(out.Issues) < p.MaxIssues {
					field := p.Field("id")
					if e == "external_identities" {
						field = "subject"
					}
					out.Issues = append(out.Issues, Issue{e, r.Ordinal, field, "PROTECTED_ENTITY_NEW"})
				}
			}
			switch kind {
			case c.New:
				count.New++
				newRows = append(newRows, PlannedRow{Ref: ref, Record: r})
			case c.Identical:
				count.Identical++
			case c.Conflict:
				count.Conflict++
			}
		}
		sorted, err := sortRows(ctx, e, newRows)
		if err != nil {
			return fail(err)
		}
		out.Rows = append(out.Rows, sorted...)
		out.Counts[string(e)] = count
		total := out.Counts["total"]
		total.Input += count.Input
		total.New += count.New
		total.Identical += count.Identical
		total.Conflict += count.Conflict
		out.Counts["total"] = total
	}
	if len(byRef) != 0 {
		return fail(errPlan)
	}
	out.IssuesTruncated = out.ErrorsTotal > len(out.Issues)
	return out, nil
}

type idHeap []string

func (h idHeap) Len() int           { return len(h) }
func (h idHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h idHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *idHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *idHeap) Pop() any          { old := *h; n := len(old); v := old[n-1]; *h = old[:n-1]; return v }
func sortRows(ctx context.Context, e p.Entity, rows []PlannedRow) ([]PlannedRow, error) {
	if e != "organizations" && e != "departments" {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Record.Values["id"].Text < rows[j].Record.Values["id"].Text })
		return rows, nil
	}
	byID := map[string]PlannedRow{}
	degrees := map[string]int{}
	children := map[string][]string{}
	for _, r := range rows {
		id := r.Record.Values["id"].Text
		if id == "" {
			return nil, errPlan
		}
		if _, ok := byID[id]; ok {
			return nil, errPlan
		}
		byID[id] = r
		degrees[id] = 0
	}
	for id, r := range byID {
		parent := r.Record.Values["parent_id"]
		if !parent.IsNull && parent.Text != "" {
			if _, exists := byID[parent.Text]; exists {
				degrees[id]++
				children[parent.Text] = append(children[parent.Text], id)
			}
		}
	}
	ready := &idHeap{}
	heap.Init(ready)
	for id, n := range degrees {
		if n == 0 {
			heap.Push(ready, id)
		}
	}
	out := []PlannedRow{}
	for ready.Len() > 0 {
		if err := p.ContextFailure(ctx); err != nil {
			return nil, err
		}
		id := heap.Pop(ready).(string)
		out = append(out, byID[id])
		for _, child := range children[id] {
			degrees[child]--
			if degrees[child] == 0 {
				heap.Push(ready, child)
			}
		}
	}
	if len(out) != len(rows) {
		return nil, errPlan
	}
	return out, nil
}
