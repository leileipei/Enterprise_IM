package importpreflight

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReuseDocumentParity(t *testing.T) {
	raw := sampleBytes(t)
	report, doc := EvaluateDocument(context.Background(), raw)
	encoded, err := EncodeReport(report)
	want, readErr := os.ReadFile("testdata/sample_report_v1.json")
	if err != nil || readErr != nil || !bytes.Equal(encoded, want) || doc == nil || *report.Counts["total"] != 74 {
		t.Fatal("report bytes or normalized document changed")
	}
	for _, tc := range mutations() {
		r, d := EvaluateDocument(context.Background(), mutateBytes(t, raw, tc.mutate))
		old := Evaluate(context.Background(), mutateBytes(t, raw, tc.mutate))
		b, _ := EncodeReport(r)
		o, _ := EncodeReport(old)
		if !bytes.Equal(b, o) || d != nil {
			t.Fatal("invalid file exposed normalized document")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, d := EvaluateDocument(ctx, raw)
	if d != nil || r.Status != "incomplete" {
		t.Fatal("canceled document")
	}
}
func TestReuseObserverBeyond200(t *testing.T) {
	var observed []Issue
	c := NewObservingCollector(func(i Issue) { observed = append(observed, i) })
	for row := 1; row <= 201; row++ {
		v := Issue{Entity: "users", Row: row, Field: "id", Code: "PK_DUPLICATE"}
		c.Add(v)
		c.Add(v)
	}
	r := BuildReport(Outcome{Status: "valid", ChecksComplete: true}, c)
	if len(observed) != 201 || r.ErrorsTotal != 201 || len(r.Issues) != 200 || !r.IssuesTruncated {
		t.Fatal("observer truncated or duplicate counted")
	}
}
func TestReuseSingleRecord(t *testing.T) {
	values := map[Field]json.RawMessage{"id": json.RawMessage(`"ABCDEF00-0000-4000-8000-000000000001"`), "tenant_id": json.RawMessage(`"00000000-0000-4000-8000-000000000001"`), "user_id": json.RawMessage(`"00000000-0000-4000-8000-000000000002"`), "organization_id": json.RawMessage(`"00000000-0000-4000-8000-000000000003"`), "effective_from": json.RawMessage(`"2026-10-07T08:00:00.123456+08:00"`)}
	r, issues, err := NormalizeRecord(context.Background(), "user_organizations", 20001, values)
	if err != nil || len(issues) != 0 || r.Ordinal != 20001 || !r.Values["employee_no"].IsNull || r.Values["is_primary"].Bool || r.Values["status"].Text != "active" || r.Values["id"].Text != "abcdef00-0000-4000-8000-000000000001" || r.Values["effective_from"].Time.Format("2006-01-02T15:04:05.999999Z07:00") != "2026-10-07T00:00:00.123456Z" {
		t.Fatal("shared normalization")
	}
	values["title"], _ = json.Marshal(strings.Repeat("x", 4097))
	_, issues, err = NormalizeRecord(context.Background(), "user_organizations", 1, values)
	if err != nil || len(issues) != 1 || issues[0].Code != "FIELD_LIMIT" {
		t.Fatal("string boundary")
	}
}
func TestReuseSchemaCopy(t *testing.T) {
	s := Schema()
	if len(s) != 9 || s[0].Entity != "tenants" || len(s[0].Fields) != 4 {
		t.Fatal("projection")
	}
	s[0].Fields[0].Name = "unsafe"
	s[0].Entity = "unsafe"
	again := Schema()
	if again[0].Entity != "tenants" || again[0].Fields[0].Name != "id" {
		t.Fatal("mutated shared schema")
	}
}
