package importpreflight

import (
	"context"
	"strings"
)

type indexedRecord struct {
	record    Record
	ambiguous bool
}
type Index struct {
	primary map[Entity]map[string]indexedRecord
}

func tuple(parts ...string) string { return strings.Join(parts, "\x00") }
func recordKey(r Record, fs []Field) (string, bool) {
	parts := make([]string, len(fs))
	for i, f := range fs {
		v, ok := r.Text(f)
		if !ok {
			return "", false
		}
		parts[i] = v
	}
	return tuple(parts...), true
}
func (i *Index) Find(e Entity, key ...string) (Record, bool) {
	v, ok := i.primary[e][tuple(key...)]
	return v.record, ok && !v.ambiguous
}
func BuildIndex(ctx context.Context, doc Document, c *Collector) (*Index, error) {
	idx := &Index{primary: map[Entity]map[string]indexedRecord{}}
	unique := map[Entity][]Field{"tenants": {"code"}, "legal_entities": {"tenant_id", "code"}, "organizations": {"tenant_id", "code"}, "departments": {"tenant_id", "organization_id", "code"}, "users": {"tenant_id", "global_employee_no"}, "external_identities": {"tenant_id", "user_id", "issuer"}}
	for _, e := range entities {
		idx.primary[e] = map[string]indexedRecord{}
		pk := []Field{"id"}
		if e == "external_identities" {
			pk = []Field{"issuer", "subject"}
		}
		keys := [][]Field{pk}
		maps := []map[string]indexedRecord{idx.primary[e]}
		if fs := unique[e]; len(fs) > 0 {
			keys = append(keys, fs)
			maps = append(maps, map[string]indexedRecord{})
		}
		for _, r := range doc.Tables[e] {
			if err := ContextFailure(ctx); err != nil {
				return idx, err
			}
			for n, fs := range keys {
				k, ok := recordKey(r, fs)
				if !ok {
					continue
				}
				old, exists := maps[n][k]
				if !exists {
					maps[n][k] = indexedRecord{record: r}
					continue
				}
				code := Code("UNIQUE_DUPLICATE")
				if n == 0 {
					code = "PK_DUPLICATE"
				}
				f := fs[len(fs)-1]
				if !old.ambiguous {
					c.Add(Issue{e, old.record.Ordinal, f, code, r.Ordinal})
				}
				c.Add(Issue{e, r.Ordinal, f, code, old.record.Ordinal})
				old.ambiguous = true
				maps[n][k] = old
			}
		}
	}
	return idx, ContextFailure(ctx)
}
