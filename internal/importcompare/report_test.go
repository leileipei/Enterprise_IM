package importcompare

import (
	"bytes"
	"context"
	"encoding/json"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
)

func validFile() p.Report {
	return p.BuildReport(p.Outcome{Status: "valid", ChecksComplete: true, Counts: zeroInputCounts()}, p.NewCollector())
}
func TestCompareUnitReportContract(t *testing.T) {
	f := validFile()
	r := BuildReport(f, "valid", true, zeroClasses(), NewCollector())
	if r.ExitCode() != 0 || r.Scope != "database_snapshot_insert_compatibility" || r.ValidationProfile != "group_identity_database_v1" || !r.DatabaseChecked || !r.ChecksComplete || r.IdentityProviderChecked || r.AdditionalDatabaseRulesChecked || r.WriteConcurrencyChecked || r.ImportAuthorized {
		t.Fatal("report contract")
	}
	invalid := BuildReport(f, "invalid", false, nil, NewCollector())
	if invalid.ExitCode() != 1 {
		t.Fatal("invalid exit")
	}
	failure := Incomplete(p.Report{}, "file", "INPUT_READ_FAILED")
	if failure.ExitCode() != 2 || failure.FileStatus != "incomplete" || failure.InputSHA256 != nil || failure.Counts["total"] != nil {
		t.Fatal("unknown input")
	}
	_, err := EncodeReport(r)
	if err != nil {
		t.Fatal("valid report rejected")
	}
	_ = context.Background()
}
func zeroInputCounts() p.Counts {
	out := p.Counts{}
	for _, k := range names() {
		v := 0
		out[k] = &v
	}
	return out
}
func zeroClasses() Classifications {
	out := Classifications{}
	for _, s := range p.Schema() {
		a, b, c := 0, 0, 0
		out[string(s.Entity)] = Classification{&a, &b, &c}
	}
	a, b, c := 0, 0, 0
	out["total"] = Classification{&a, &b, &c}
	return out
}
func TestCompareUnitIncompleteCounts(t *testing.T) {
	for _, status := range []p.Status{"invalid", "incomplete"} {
		r := BuildReport(validFile(), status, false, zeroClasses(), NewCollector())
		if len(r.ClassificationCounts) != 10 {
			t.Fatal("missing classifications")
		}
		for _, n := range r.ClassificationCounts {
			if n.New != nil || n.Identical != nil || n.Conflict != nil {
				t.Fatal("partial counts escaped")
			}
		}
		if r.DatabaseChecked || r.ChecksComplete {
			t.Fatal("partial complete flag")
		}
	}
}
func TestCompareUnitReportPrivacy(t *testing.T) {
	r := BuildReport(validFile(), "invalid", false, nil, NewCollector())
	r.Issues = []Issue{{Stage: "database", Issue: p.Issue{Entity: "users", Row: 1, Field: "id", Code: "secret-marker"}}}
	r.ErrorsTotal = 1
	if _, err := EncodeReport(r); err == nil {
		t.Fatal("unknown code leaked")
	}
	r.Issues[0].Issue.Code = "DATABASE_READ_FAILED"
	r.Issues[0].Stage = "secret-marker"
	if _, err := EncodeReport(r); err == nil {
		t.Fatal("unknown stage leaked")
	}
}
func TestCompareUnitReportTruncation(t *testing.T) {
	c := NewCollector()
	for n := 201; n > 0; n-- {
		i := Issue{Stage: "database", Issue: p.Issue{Entity: "users", Row: n, Field: "id", Code: "STORED_VALUE_DIFFERS"}}
		c.Add(i)
		c.Add(i)
	}
	c.Add(Issue{Stage: "file", Issue: p.Issue{Entity: "document", Field: "_document", Code: "INPUT_READ_FAILED"}})
	r := BuildReport(validFile(), "invalid", false, nil, c)
	if r.ErrorsTotal != 202 || len(r.Issues) != 200 || !r.IssuesTruncated || r.Issues[0].Stage != "file" || r.Issues[199].Issue.Row != 199 {
		t.Fatal("bounded stable ordered issues")
	}
	a, e := EncodeReport(r)
	b, e2 := EncodeReport(r)
	if e != nil || e2 != nil || !bytes.Equal(a, b) {
		t.Fatal("unstable report")
	}
	var m map[string]any
	if json.Unmarshal(a, &m) != nil || len(m) != 18 {
		t.Fatal("report fields")
	}
}

func TestCompareUnitReportPreservesTruncatedFileTotal(t *testing.T) {
	c := p.NewCollector()
	for row := 1; row <= 201; row++ {
		c.Add(p.Issue{Entity: "users", Row: row, Field: "id", Code: "PK_DUPLICATE"})
	}
	f := p.BuildReport(p.Outcome{Status: "valid", ChecksComplete: true}, c)
	r := Incomplete(f, "file", "CANCELED")
	if r.ErrorsTotal != 202 {
		t.Fatal("runtime error lost after file truncation")
	}
}
