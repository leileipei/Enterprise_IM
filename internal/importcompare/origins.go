package importcompare

import p "github.com/leileipei/Enterprise_IM/internal/importpreflight"

type origin struct {
	InputRows []int
	Stored    bool
}
type origins map[RowRef]origin

func (o origins) project(i p.Issue, c *Collector, conflicts map[RowRef]bool) bool {
	a := o[RowRef{i.Entity, i.Row}]
	b := o[RowRef{i.Entity, i.RelatedRow}]
	any := false
	add := func(rows []int, related []int, stored bool) {
		for _, row := range rows {
			v := i
			v.Row = row
			v.RelatedRow = 0
			if !stored && len(related) > 0 {
				v.RelatedRow = related[0]
			}
			c.Add(Issue{"database", v})
			conflicts[RowRef{i.Entity, row}] = true
			any = true
		}
	}
	add(a.InputRows, b.InputRows, a.Stored || b.Stored)
	if i.RelatedRow > 0 {
		add(b.InputRows, a.InputRows, a.Stored || b.Stored)
	}
	return any
}
