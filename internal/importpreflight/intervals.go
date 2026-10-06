package importpreflight

import (
	"context"
	"sort"
	"time"
)

type interval struct {
	record Record
	from   time.Time
	to     *time.Time
}

func validInterval(r Record) (interval, bool) {
	start, a := r.Time("effective_from")
	end, b := r.Time("effective_to")
	if !a || !b || start == nil || end != nil && !end.After(*start) {
		return interval{}, false
	}
	return interval{r, *start, end}, true
}
func CheckIntervals(ctx context.Context, doc Document, idx *Index, relations Relations, c *Collector) error {
	for _, e := range []Entity{"user_organizations", "user_departments", "admin_grants"} {
		groups := map[string][]interval{}
		primary := map[string][]interval{}
		for _, r := range doc.Tables[e] {
			if er := ContextFailure(ctx); er != nil {
				return er
			}
			iv, ok := validInterval(r)
			if !ok {
				a, va := r.Time("effective_from")
				b, vb := r.Time("effective_to")
				if va && vb && a != nil && b != nil {
					c.Add(Issue{Entity: e, Row: r.Ordinal, Field: "effective_to", Code: "INTERVAL_INVALID"})
				}
				continue
			}
			if e == "admin_grants" {
				continue
			}
			fs := []Field{"tenant_id", "user_id", "organization_id"}
			if e == "user_departments" {
				fs = []Field{"tenant_id", "user_organization_id", "department_id"}
			}
			ready := true
			for _, f := range fs {
				if !relations.Valid(e, r.Ordinal, f) {
					ready = false
				}
			}
			if e == "user_departments" && !relations.Valid(e, r.Ordinal, "organization_id") {
				ready = false
			}
			if !ready {
				continue
			}
			key, valid := recordKey(r, fs)
			if !valid {
				continue
			}
			groups[key] = append(groups[key], iv)
			if isPrimary, valid := r.Bool("is_primary"); valid && isPrimary {
				key, _ := recordKey(r, fs[:2])
				primary[key] = append(primary[key], iv)
			}
			if e == "user_departments" {
				id, _ := r.Text("user_organization_id")
				parent, found := idx.Find("user_organizations", id)
				p, valid := validInterval(parent)
				if found && valid && (iv.from.Before(p.from) || p.to != nil && (iv.to == nil || iv.to.After(*p.to))) {
					c.Add(Issue{Entity: e, Row: r.Ordinal, Field: "effective_from", Code: "DEPARTMENT_INTERVAL_OUTSIDE"})
				}
			}
		}
		for n, all := range []map[string][]interval{groups, primary} {
			code := Code("INTERVAL_OVERLAP")
			if n == 1 {
				code = "PRIMARY_OVERLAP"
			}
			for _, items := range all {
				if er := ContextFailure(ctx); er != nil {
					return er
				}
				sort.Slice(items, func(i, j int) bool {
					if items[i].from.Equal(items[j].from) {
						return items[i].record.Ordinal < items[j].record.Ordinal
					}
					return items[i].from.Before(items[j].from)
				})
				if len(items) == 0 {
					continue
				}
				max := items[0]
				for _, v := range items[1:] {
					if er := ContextFailure(ctx); er != nil {
						return er
					}
					if max.to == nil || v.from.Before(*max.to) {
						c.Add(Issue{Entity: e, Row: v.record.Ordinal, Field: "effective_from", Code: code, RelatedRow: max.record.Ordinal})
					}
					if max.to != nil && (v.to == nil || v.to.After(*max.to)) {
						max = v
					}
				}
			}
		}
	}
	return ContextFailure(ctx)
}
