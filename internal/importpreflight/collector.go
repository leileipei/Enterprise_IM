package importpreflight

import (
	"container/heap"
	"sort"
)

func less(a, b Issue) bool {
	if entityRank(a.Entity) != entityRank(b.Entity) {
		return entityRank(a.Entity) < entityRank(b.Entity)
	}
	if a.Row != b.Row {
		return a.Row < b.Row
	}
	if fieldRank(a.Entity, a.Field) != fieldRank(b.Entity, b.Field) {
		return fieldRank(a.Entity, a.Field) < fieldRank(b.Entity, b.Field)
	}
	if a.Code != b.Code {
		return a.Code < b.Code
	}
	return a.RelatedRow < b.RelatedRow
}

type issueHeap []Issue

func (h issueHeap) Len() int           { return len(h) }
func (h issueHeap) Less(i, j int) bool { return less(h[j], h[i]) }
func (h issueHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *issueHeap) Push(v any)        { *h = append(*h, v.(Issue)) }
func (h *issueHeap) Pop() any          { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }

type Collector struct {
	seen    map[Issue]struct{}
	items   issueHeap
	total   int
	observe func(Issue)
}

func NewCollector() *Collector { return &Collector{seen: make(map[Issue]struct{})} }
func (c *Collector) Add(v Issue) {
	if _, ok := c.seen[v]; ok {
		return
	}
	c.seen[v] = struct{}{}
	c.total++
	if c.observe != nil {
		c.observe(v)
	}
	if len(c.items) < MaxIssues {
		heap.Push(&c.items, v)
	} else if less(v, c.items[0]) {
		c.items[0] = v
		heap.Fix(&c.items, 0)
	}
}
func (c *Collector) sorted() []Issue {
	out := append([]Issue{}, c.items...)
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}
