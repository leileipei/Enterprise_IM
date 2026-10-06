package importpreflight

import (
	"testing"
)

func TestScalarsUUIDTextAndTime(t *testing.T) {
	for _, s := range []string{"AE5368D5-748F-56C0-A4F8-1D486BA4E6CA", "00000000-0000-0000-0000-000000000000"} {
		v, ok := normalizeUUID(s)
		if !ok || len(v) != 36 {
			t.Fatal("valid UUID rejected")
		}
	}
	v, _ := normalizeUUID("AE5368D5-748F-56C0-A4F8-1D486BA4E6CA")
	if v != "ae5368d5-748f-56c0-a4f8-1d486ba4e6ca" {
		t.Fatal("case")
	}
	for _, s := range []string{"ae5368d5748f56c0a4f81d486ba4e6ca", "ae5368d5-748f-56c0-a4f8-1d486ba4e6cg", "{ae5368d5-748f-56c0-a4f8-1d486ba4e6ca}"} {
		if _, ok := normalizeUUID(s); ok {
			t.Fatal("invalid UUID")
		}
	}
	for _, s := range []string{"0001-01-01T00:00:00Z", "9999-12-31T23:59:59.999999Z", "2024-02-29T01:02:03.123456+08:00"} {
		if _, ok := parseTimestamp(s); !ok {
			t.Fatalf("valid time %s", s)
		}
	}
	for _, s := range []string{"2026-10-01T00:00:00.0000001Z", "2026-02-29T00:00:00Z", "0000-01-01T00:00:00Z", "2026-10-01T00:00:00", "2026-10-01T00:00:00+24:00", "2026-10-01T00:00:00+00:60", "2026-10-01T00:00:00,1Z", "2026-10-01t00:00:00z"} {
		if _, ok := parseTimestamp(s); ok {
			t.Fatalf("invalid time accepted %s", s)
		}
	}
	for _, s := range []string{"   ", "\u0085", " A ", "e\u0301"} {
		b := mutateBytes(t, sampleBytes(t), func(m map[string]any) { tableRows(m, "external_identities")[0].(map[string]any)["subject"] = s })
		d, _, _, r := normalizeBytes(t, b)
		if hasCode(r, "TEXT_BLANK") != (s == "   ") {
			t.Fatal("btrim mismatch")
		}
		if s != "   " {
			v, ok := d.Tables["external_identities"][0].Text("subject")
			if !ok || v != s {
				t.Fatal("text normalized")
			}
		}
	}
	b := mutateBytes(t, sampleBytes(t), func(m map[string]any) { tableRows(m, "users")[0].(map[string]any)["display_name"] = "" })
	_, _, _, r := normalizeBytes(t, b)
	if r.ErrorsTotal != 0 {
		t.Fatal("empty ordinary text rejected")
	}
}
