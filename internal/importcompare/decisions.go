package importcompare

import (
	"context"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
)

type DecisionKind string

const (
	New       DecisionKind = "new"
	Identical DecisionKind = "identical"
	Conflict  DecisionKind = "conflict"
)

type RowDecision struct {
	Ref  RowRef
	Kind DecisionKind
}

// Compare preserves the existing public counts; all per-row decisions are shared with append planning.
func Compare(ctx context.Context, input p.Document, snapshot Snapshot) (Classifications, *Collector, error) {
	decisions, issues, err := Decide(ctx, input, snapshot)
	if err != nil {
		return nil, nil, err
	}
	counts := map[string][3]int{}
	for _, schema := range p.Schema() {
		counts[string(schema.Entity)] = [3]int{}
	}
	for _, decision := range decisions {
		k := string(decision.Ref.Entity)
		v := counts[k]
		switch decision.Kind {
		case New:
			v[0]++
		case Identical:
			v[1]++
		case Conflict:
			v[2]++
		}
		counts[k] = v
		v = counts["total"]
		switch decision.Kind {
		case New:
			v[0]++
		case Identical:
			v[1]++
		case Conflict:
			v[2]++
		}
		counts["total"] = v
	}
	classes := Classifications{}
	if _, ok := counts["total"]; !ok {
		counts["total"] = [3]int{}
	}
	for k, v := range counts {
		a, b, c := v[0], v[1], v[2]
		classes[k] = Classification{&a, &b, &c}
	}
	return classes, issues, nil
}
