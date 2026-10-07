package importcompare

import p "github.com/leileipei/Enterprise_IM/internal/importpreflight"

// The original collectors preserve table/field ordering and bound retained diagnostics.
type Collector struct {
	file, database *p.Collector
	fileHidden     int
}

func NewCollector() *Collector { return &Collector{file: p.NewCollector(), database: p.NewCollector()} }
func (c *Collector) Add(i Issue) {
	if i.Stage == "file" {
		c.file.Add(i.Issue)
	} else {
		c.database.Add(i.Issue)
	}
}
func (c *Collector) parts() (p.Report, p.Report) {
	o := p.Outcome{Status: "valid", ChecksComplete: true}
	return p.BuildReport(o, c.file), p.BuildReport(o, c.database)
}
func (c *Collector) Issues() []Issue {
	f, d := c.parts()
	out := []Issue{}
	for _, i := range f.Issues {
		out = append(out, Issue{"file", i})
	}
	for _, i := range d.Issues {
		if len(out) == p.MaxIssues {
			break
		}
		out = append(out, Issue{"database", i})
	}
	return out
}
func (c *Collector) Total() int {
	f, d := c.parts()
	return f.ErrorsTotal + c.fileHidden + d.ErrorsTotal
}
