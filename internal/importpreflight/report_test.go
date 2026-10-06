package importpreflight

import (
	"encoding/json"
	"strings"
	"testing"
)

func hasCode(r Report, code Code) bool {
	for _, i := range r.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}
func TestReportBoundaryAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		s        Status
		complete bool
		code     int
	}{{"valid", true, 0}, {"invalid", false, 1}, {"incomplete", false, 2}} {
		r := BuildReport(Outcome{Status: tc.s, ChecksComplete: tc.complete}, NewCollector())
		b, e := EncodeReport(r)
		if e != nil || r.ExitCode() != tc.code {
			t.Fatalf("status %s: %v", tc.s, e)
		}
		var v map[string]any
		if json.Unmarshal(b, &v) != nil {
			t.Fatal("bad JSON")
		}
		if v["report_version"] != float64(1) || v["validation_profile"] != "group_identity_v1" || v["scope"] != "offline_file" || v["database_checked"] != false || v["identity_provider_checked"] != false || v["import_authorized"] != false {
			t.Fatal("incorrect trust boundaries")
		}
		counts := v["counts"].(map[string]any)
		if len(counts) != 10 || counts["total"] != nil {
			t.Fatal("unknown counts represented as zero")
		}
		if v["input_sha256"] != nil || len(v["issues"].([]any)) != 0 {
			t.Fatal("nil encoding")
		}
	}
	c := NewCollector()
	c.Add(Issue{Entity: "document", Field: "_document", Code: "JSON_INVALID"})
	if BuildReport(Outcome{Status: "valid", ChecksComplete: true}, c).ExitCode() == 0 {
		t.Fatal("error accepted as valid")
	}
}
func TestReportNoDynamicValues(t *testing.T) {
	c := NewCollector()
	c.Add(Issue{Entity: "users", Row: 1, Field: "id", Code: "UUID_INVALID"})
	b, _ := EncodeReport(BuildReport(Outcome{Status: "invalid", ChecksComplete: true}, c))
	var v map[string]any
	json.Unmarshal(b, &v)
	issue := v["issues"].([]any)[0].(map[string]any)
	if len(issue) != 4 || issue["row"] != float64(1) {
		t.Fatal("unexpected issue content")
	}
	c.Add(Issue{Entity: "document", Field: "_document", Code: "JSON_INVALID"})
	b, _ = EncodeReport(BuildReport(Outcome{Status: "invalid"}, c))
	json.Unmarshal(b, &v)
	if v["issues"].([]any)[0].(map[string]any)["row"] != nil {
		t.Fatal("document row must be null")
	}
}
func TestReportEncodingBound(t *testing.T) {
	r := BuildReport(Outcome{Status: "invalid"}, NewCollector())
	r.Issues = make([]Issue, 20000)
	for i := range r.Issues {
		r.Issues[i] = Issue{Entity: "user_organizations", Row: i + 1, Field: "organization_id", Code: "REF_SCOPE_MISMATCH"}
	}
	b, e := EncodeReport(r)
	if e == nil || b != nil || strings.Contains(e.Error(), "organization_id") {
		t.Fatal("oversized output accepted or leaked")
	}
}
