package importpreflight

import "context"

func CheckGraphs(ctx context.Context, doc Document, idx *Index, relations Relations, c *Collector) error {
	for _, e := range []Entity{"organizations", "departments"} {
		edges := map[string]string{}
		rows := map[string]int{}
		for _, r := range doc.Tables[e] {
			if er := ContextFailure(ctx); er != nil {
				return er
			}
			id, valid := r.Text("id")
			if !valid {
				continue
			}
			if _, ok := idx.Find(e, id); !ok {
				continue
			}
			rows[id] = r.Ordinal
			if !relations.Valid(e, r.Ordinal, "parent_id") {
				continue
			}
			parent, _ := r.Text("parent_id")
			if parent == id {
				c.Add(Issue{Entity: e, Row: r.Ordinal, Field: "parent_id", Code: "SELF_PARENT"})
				continue
			}
			edges[id] = parent
		}
		done := map[string]bool{}
		for _, r := range doc.Tables[e] {
			id, valid := r.Text("id")
			if !valid || done[id] {
				continue
			}
			path := []string{}
			positions := map[string]int{}
			cur := id
			for cur != "" && !done[cur] {
				if er := ContextFailure(ctx); er != nil {
					return er
				}
				if start, ok := positions[cur]; ok {
					for _, node := range path[start:] {
						c.Add(Issue{Entity: e, Row: rows[node], Field: "parent_id", Code: "TREE_CYCLE"})
					}
					break
				}
				positions[cur] = len(path)
				path = append(path, cur)
				cur = edges[cur]
			}
			for _, node := range path {
				done[node] = true
			}
		}
	}
	return ContextFailure(ctx)
}
