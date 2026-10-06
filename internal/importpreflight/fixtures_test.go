package importpreflight

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func sampleBytes(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/sample_data_v1.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func sampleDocument(t *testing.T) Document {
	t.Helper()
	c := NewCollector()
	raw, ok, e := DecodeRaw(context.Background(), sampleBytes(t), c)
	if !ok || e != nil {
		t.Fatal("sample parse")
	}
	d, _, ok, e := Normalize(context.Background(), raw, c)
	if !ok || e != nil || c.total != 0 {
		t.Fatalf("sample normalize: %v %+v", e, c.sorted())
	}
	return d
}
func fixtureRows(t *testing.T, total int) []byte {
	t.Helper()
	var d map[string]any
	json.Unmarshal([]byte(emptyInput()), &d)
	tabs := d["tables"].(map[string]any)
	tenant := "00000000-0000-0000-0000-000000000001"
	tabs["tenants"] = []any{map[string]any{"id": tenant, "code": "t", "name": "t"}}
	rows := []any{}
	for i := 1; i < total; i++ {
		rows = append(rows, map[string]any{"id": fmt.Sprintf("00000000-0000-0000-0001-%012x", i), "tenant_id": tenant, "global_employee_no": fmt.Sprint(i), "display_name": "synthetic"})
	}
	tabs["users"] = rows
	b, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func normalizeBytes(t *testing.T, b []byte) (Document, Counts, bool, Report) {
	t.Helper()
	c := NewCollector()
	raw, ok, e := DecodeRaw(context.Background(), b, c)
	if e != nil {
		t.Fatal(e)
	}
	if !ok {
		return Document{}, nil, false, BuildReport(Outcome{Status: "invalid"}, c)
	}
	d, n, ok, e := Normalize(context.Background(), raw, c)
	if e != nil {
		t.Fatal(e)
	}
	return d, n, ok, BuildReport(Outcome{Status: "invalid"}, c)
}
func mutateBytes(t *testing.T, b []byte, fn func(map[string]any)) []byte {
	t.Helper()
	var d map[string]any
	if json.Unmarshal(b, &d) != nil {
		t.Fatal("fixture JSON")
	}
	fn(d)
	b, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func tableRows(d map[string]any, e string) []any { return d["tables"].(map[string]any)[e].([]any) }
