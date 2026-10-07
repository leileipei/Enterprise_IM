package importpreflight

import (
	"context"
	"encoding/json"
)

// FieldSchema describes the existing closed projection, without exposing mutable specifications.
type FieldSchema struct {
	Name     Field
	Kind     string
	Nullable bool
}
type TableSchema struct {
	Entity Entity
	Fields []FieldSchema
}

func Schema() []TableSchema {
	out := make([]TableSchema, 0, len(entities))
	for _, e := range entities {
		t := TableSchema{Entity: e}
		for _, f := range fields[e] {
			s := specification(e, f)
			t.Fields = append(t.Fields, FieldSchema{f, s.kind, s.nullable})
		}
		out = append(out, t)
	}
	return out
}
func NormalizeRecord(ctx context.Context, e Entity, ordinal int, values map[Field]json.RawMessage) (Record, []Issue, error) {
	r := Record{Ordinal: ordinal, Values: map[Field]Value{}}
	issues := []Issue{}
	if _, ok := knownEntity(string(e)); !ok {
		return r, []Issue{{Entity: "document", Field: "_record", Code: "TYPE_INVALID"}}, ContextFailure(ctx)
	}
	for _, f := range fields[e] {
		if err := ContextFailure(ctx); err != nil {
			return r, issues, err
		}
		b, present := values[f]
		v, code := normalizeValue(b, present, specification(e, f))
		r.Values[f] = v
		if code != "" {
			issues = append(issues, Issue{Entity: e, Row: ordinal, Field: f, Code: code})
		}
	}
	return r, issues, ContextFailure(ctx)
}
func ValidateModel(ctx context.Context, doc Document, c *Collector) error {
	idx, err := BuildIndex(ctx, doc, c)
	if err != nil {
		return err
	}
	rels, err := CheckReferences(ctx, doc, idx, c)
	if err != nil {
		return err
	}
	if err = CheckGraphs(ctx, doc, idx, rels, c); err != nil {
		return err
	}
	if err = CheckIntervals(ctx, doc, idx, rels, c); err != nil {
		return err
	}
	return ContextFailure(ctx)
}
func NewObservingCollector(observe func(Issue)) *Collector {
	c := NewCollector()
	c.observe = observe
	return c
}
