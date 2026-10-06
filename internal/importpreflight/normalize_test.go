package importpreflight

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeSampleAndDefaults(t *testing.T) {
	_, n, ok, r := normalizeBytes(t, sampleBytes(t))
	if !ok || r.ErrorsTotal != 0 || *n["total"] != 74 {
		t.Fatal("sample")
	}
	num := 0
	for _, e := range entities {
		num += len(fields[e])
	}
	if num != 62 {
		t.Fatal("incomplete schema")
	}
	d := sampleDocument(t)
	b := mutateBytes(t, sampleBytes(t), func(m map[string]any) {
		for _, e := range entities {
			for _, v := range tableRows(m, string(e)) {
				row := v.(map[string]any)
				if row["status"] == "active" {
					delete(row, "status")
				}
				if row["is_primary"] == false {
					delete(row, "is_primary")
				}
				for k, v := range row {
					if v == nil {
						delete(row, k)
					}
				}
			}
		}
	})
	e, _, ok, r := normalizeBytes(t, b)
	if !ok || r.ErrorsTotal != 0 || !reflect.DeepEqual(d, e) {
		t.Fatal("defaults or null presence changed meaning")
	}
	if _, ok := knownField("users", "created_at"); ok {
		t.Fatal("generated column accepted")
	}
}
func TestNormalizeNullAndTypes(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
		code  Code
	}{{"status", nil, "TYPE_INVALID"}, {"id", true, "TYPE_INVALID"}, {"tenant_id", nil, "TYPE_INVALID"}, {"display_name", []any{}, "TYPE_INVALID"}, {"display_name", "x\x00y", "TEXT_NUL"}} {
		b := mutateBytes(t, sampleBytes(t), func(m map[string]any) { tableRows(m, "users")[0].(map[string]any)[tc.field] = tc.value })
		d, _, ok, r := normalizeBytes(t, b)
		if !ok || !hasCode(r, tc.code) || d.Tables["users"][0].Values[Field(tc.field)].Valid {
			t.Fatalf("field %s %+v", tc.field, r.Issues)
		}
	}
	b := mutateBytes(t, sampleBytes(t), func(m map[string]any) { delete(tableRows(m, "users")[0].(map[string]any), "id") })
	_, _, ok, r := normalizeBytes(t, b)
	if !ok || !hasCode(r, "FIELD_REQUIRED") {
		t.Fatal("required")
	}
	for _, s := range []string{strings.Replace(emptyInput(), `"format_version":1`, `"format_version":1.0`, 1), strings.Replace(emptyInput(), `"baseline_commit":"80424dbd5a537023c33e56654f4a12b522885a21"`, `"baseline_commit":"bad"`, 1), strings.Replace(emptyInput(), `"identity_source_selected":false`, `"identity_source_selected":null`, 1)} {
		_, _, ok, r := normalizeBytes(t, []byte(s))
		if ok || r.ErrorsTotal == 0 {
			t.Fatal("metadata accepted")
		}
	}
	_ = json.Valid
}
func TestStringByteBudget(t *testing.T) {
	for _, s := range []string{strings.Repeat("a", 4096), strings.Repeat("a", 4097), strings.Repeat("界", 1365) + "a", strings.Repeat("界", 1366)} {
		b := mutateBytes(t, sampleBytes(t), func(m map[string]any) { tableRows(m, "users")[0].(map[string]any)["display_name"] = s })
		_, _, ok, r := normalizeBytes(t, b)
		if ok != (len(s) <= 4096) || hasCode(r, "FIELD_LIMIT") != (len(s) > 4096) {
			t.Fatalf("byte length %d", len(s))
		}
	}
}
