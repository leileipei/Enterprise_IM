package importcompare

import (
	"context"
	"encoding/json"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"strings"
	"time"
)

var businessKeys = map[p.Entity][]p.Field{
	"tenants": {"code"}, "legal_entities": {"tenant_id", "code"}, "organizations": {"tenant_id", "code"}, "departments": {"tenant_id", "organization_id", "code"}, "users": {"tenant_id", "global_employee_no"}, "external_identities": {"tenant_id", "user_id", "issuer"},
}

func pk(e p.Entity) []p.Field {
	if e == "external_identities" {
		return []p.Field{"issuer", "subject"}
	}
	return []p.Field{"id"}
}
func key(r p.Record, fields []p.Field) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = r.Values[f].Text
	}
	return strings.Join(out, "\x00")
}
func equal(a, b p.Value) bool {
	if a.Valid != b.Valid || a.IsNull != b.IsNull || a.Text != b.Text || a.Bool != b.Bool {
		return false
	}
	if a.Time == nil || b.Time == nil {
		return a.Time == nil && b.Time == nil
	}
	return a.Time.Equal(*b.Time)
}
func rawRecord(r p.Record, s p.TableSchema) (map[p.Field]json.RawMessage, error) {
	out := map[p.Field]json.RawMessage{}
	for _, f := range s.Fields {
		v, exists := r.Values[f.Name]
		if !exists || !v.Valid {
			return nil, p.Failure{Code: "DATABASE_DATA_UNSUPPORTED"}
		}
		var x any
		if !v.IsNull {
			switch f.Kind {
			case "bool":
				x = v.Bool
			case "time":
				if v.Time == nil || v.Time.UTC().Year() < 1 || v.Time.UTC().Year() > 9999 {
					return nil, p.Failure{Code: "DATABASE_DATA_UNSUPPORTED"}
				}
				x = v.Time.UTC().Format(time.RFC3339Nano)
			default:
				x = v.Text
			}
		}
		b, err := json.Marshal(x)
		if err != nil {
			return nil, p.Failure{Code: "DATABASE_DATA_UNSUPPORTED"}
		}
		out[f.Name] = b
	}
	return out, nil
}
func checkStored(ctx context.Context, d p.Document) error {
	total, bytes := 0, 0
	for _, s := range p.Schema() {
		for _, r := range d.Tables[s.Entity] {
			if err := p.ContextFailure(ctx); err != nil {
				return err
			}
			total++
			if total > 20000 {
				return p.Failure{Code: "DATABASE_LIMIT"}
			}
			raw, err := rawRecord(r, s)
			if err != nil {
				return err
			}
			normalized, issues, err := p.NormalizeRecord(ctx, s.Entity, r.Ordinal, raw)
			if err != nil {
				return err
			}
			if len(issues) > 0 {
				return p.Failure{Code: "DATABASE_DATA_UNSUPPORTED"}
			}
			for _, f := range s.Fields {
				if !equal(r.Values[f.Name], normalized.Values[f.Name]) {
					return p.Failure{Code: "DATABASE_DATA_UNSUPPORTED"}
				}
				v := r.Values[f.Name]
				bytes += len(v.Text)
				if v.Time != nil {
					bytes += len(v.Time.UTC().Format(time.RFC3339Nano))
				}
				if f.Kind == "bool" && !v.IsNull {
					if v.Bool {
						bytes += 4
					} else {
						bytes += 5
					}
				}
			}
			if bytes > 64*1024*1024 {
				return p.Failure{Code: "DATABASE_LIMIT"}
			}
		}
	}
	c := p.NewCollector()
	if err := p.ValidateModel(ctx, d, c); err != nil {
		return err
	}
	if p.BuildReport(p.Outcome{Status: "valid", ChecksComplete: true}, c).ErrorsTotal > 0 {
		return p.Failure{Code: "DATABASE_DATA_INVALID"}
	}
	return nil
}
func Decide(ctx context.Context, input p.Document, snapshot Snapshot) ([]RowDecision, *Collector, error) {
	fail := func(err error) ([]RowDecision, *Collector, error) { return nil, nil, err }
	if err := p.ContextFailure(ctx); err != nil {
		return fail(err)
	}
	if !snapshot.TenantFound {
		return fail(p.Failure{Code: "DATABASE_DATA_INVALID"})
	}
	if err := checkStored(ctx, snapshot.Data); err != nil {
		return fail(err)
	}
	merged := p.Document{Tables: map[p.Entity][]p.Record{}}
	sources := origins{}
	conflicts := map[RowRef]bool{}
	identical := map[RowRef]bool{}
	c := NewCollector()
	inputTotal := 0
	mergedTotal := 0
	for _, s := range p.Schema() {
		e := s.Entity
		byPK := map[string]int{}
		unique := map[string]bool{}
		merged.Tables[e] = []p.Record{}
		for _, r := range snapshot.Data.Tables[e] {
			v := r
			v.Ordinal = len(merged.Tables[e]) + 1
			merged.Tables[e] = append(merged.Tables[e], v)
			byPK[key(v, pk(e))] = v.Ordinal
			sources[RowRef{e, v.Ordinal}] = origin{Stored: true}
			if fs := businessKeys[e]; len(fs) > 0 {
				unique[key(v, fs)] = true
			}
			mergedTotal++
		}
		for _, r := range input.Tables[e] {
			if err := p.ContextFailure(ctx); err != nil {
				return fail(err)
			}
			inputTotal++
			if inputTotal > p.MaxRows || r.Ordinal < 1 || r.Ordinal > p.MaxRows {
				return fail(p.Failure{Code: "DATABASE_LIMIT"})
			}
			ref := RowRef{e, r.Ordinal}
			if snapshot.GlobalKeys[ref] {
				conflicts[ref] = true
				c.Add(Issue{"database", p.Issue{Entity: e, Row: r.Ordinal, Field: pk(e)[len(pk(e))-1], Code: "GLOBAL_KEY_CONFLICT"}})
				continue
			}
			if ordinal, ok := byPK[key(r, pk(e))]; ok {
				stored := merged.Tables[e][ordinal-1]
				same := true
				for _, f := range s.Fields {
					if !equal(r.Values[f.Name], stored.Values[f.Name]) {
						same = false
						c.Add(Issue{"database", p.Issue{Entity: e, Row: r.Ordinal, Field: f.Name, Code: "STORED_VALUE_DIFFERS"}})
					}
				}
				if !same {
					conflicts[ref] = true
					continue
				}
				identical[ref] = true
				o := sources[RowRef{e, ordinal}]
				o.InputRows = append(o.InputRows, r.Ordinal)
				sources[RowRef{e, ordinal}] = o
				continue
			}
			if fs := businessKeys[e]; len(fs) > 0 && unique[key(r, fs)] {
				conflicts[ref] = true
				c.Add(Issue{"database", p.Issue{Entity: e, Row: r.Ordinal, Field: fs[len(fs)-1], Code: "STORED_UNIQUE_CONFLICT"}})
				continue
			}
			v := r
			v.Ordinal = len(merged.Tables[e]) + 1
			merged.Tables[e] = append(merged.Tables[e], v)
			sources[RowRef{e, v.Ordinal}] = origin{InputRows: []int{r.Ordinal}}
			mergedTotal++
			if mergedTotal > 30000 {
				return fail(p.Failure{Code: "DATABASE_LIMIT"})
			}
		}
	}
	unlocated := false
	observe := p.NewObservingCollector(func(i p.Issue) {
		if !sources.project(i, c, conflicts) {
			unlocated = true
		}
	})
	if err := p.ValidateModel(ctx, merged, observe); err != nil {
		return fail(err)
	}
	if unlocated {
		return fail(p.Failure{Code: "DATABASE_DATA_INVALID"})
	}
	decisions := []RowDecision{}
	for _, schema := range p.Schema() {
		for _, row := range input.Tables[schema.Entity] {
			ref := RowRef{schema.Entity, row.Ordinal}
			kind := New
			if conflicts[ref] {
				kind = Conflict
			} else if identical[ref] {
				kind = Identical
			}
			decisions = append(decisions, RowDecision{Ref: ref, Kind: kind})
		}
	}
	if err := p.ContextFailure(ctx); err != nil {
		return fail(err)
	}
	return decisions, c, nil
}
