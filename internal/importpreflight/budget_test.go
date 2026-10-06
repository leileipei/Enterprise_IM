package importpreflight

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestPreflightResourceBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := []byte(emptyInput())
	b := append(base, bytes.Repeat([]byte(" "), MaxInput-len(base))...)
	r := Evaluate(ctx, b)
	if r.ExitCode() != 0 {
		t.Fatalf("10 MiB boundary %+v", r.Issues)
	}
	r = Evaluate(ctx, append(b, ' '))
	if r.ExitCode() != 1 || r.ChecksComplete || r.InputSHA256 != nil || !hasCode(r, "INPUT_TOO_LARGE") {
		t.Fatal("byte limit")
	}
	for _, n := range []int{10000, 10001} {
		r = Evaluate(ctx, fixtureRows(t, n))
		if n == 10000 {
			if r.ExitCode() != 0 || *r.Counts["total"] != 10000 {
				t.Fatalf("rows boundary %+v", r.Issues)
			}
		} else if r.ExitCode() != 1 || r.ChecksComplete || !hasCode(r, "ROW_LIMIT") {
			t.Fatal("row limit")
		}
	}
	for _, n := range []int{16, 17} {
		r = Evaluate(ctx, []byte(strings.Repeat("[", n)+"0"+strings.Repeat("]", n)))
		if hasCode(r, "DEPTH_LIMIT") != (n == 17) {
			t.Fatal("depth boundary")
		}
	}
	for _, n := range []int{4096, 4097} {
		r = Evaluate(ctx, mutateBytes(t, sampleBytes(t), func(m map[string]any) {
			tableRows(m, "users")[0].(map[string]any)["display_name"] = strings.Repeat("x", n)
		}))
		if n == 4096 {
			if r.ExitCode() != 0 {
				t.Fatal("text boundary")
			}
		} else if r.ExitCode() != 1 || r.ChecksComplete || !hasCode(r, "FIELD_LIMIT") {
			t.Fatal("text limit")
		}
	}
}
func TestPreflightDiagnosticsOver200(t *testing.T) {
	b := mutateBytes(t, fixtureRows(t, 401), func(m map[string]any) {
		for _, v := range tableRows(m, "users") {
			v.(map[string]any)["tenant_id"] = "00000000-0000-0000-0000-000000000999"
		}
	})
	r := Evaluate(context.Background(), b)
	if r.ExitCode() != 1 || !r.ChecksComplete || r.ErrorsTotal != 400 || len(r.Issues) != 200 || !r.IssuesTruncated {
		t.Fatalf("truncated %+v", r)
	}
	for i, v := range r.Issues {
		if v.Entity != "users" || v.Row != i+1 || v.Field != "tenant_id" || v.Code != "REF_NOT_FOUND" {
			t.Fatal("wrong retained position")
		}
	}
	a, _ := EncodeReport(r)
	for i := 0; i < 4; i++ {
		again, _ := EncodeReport(Evaluate(context.Background(), b))
		if !bytes.Equal(a, again) {
			t.Fatal("unstable truncated output")
		}
	}
}
func TestPreflightDuplicateKeysStopAtRowLimit(t *testing.T) {
	duplicateRows := func(n int) string { return strings.Repeat(`{"id":0,"id":0},`, n-1) + `{"id":0,"id":0}` }
	for _, tc := range []struct {
		name         string
		users, legal int
		limit        bool
		total        int
	}{{"exact", 10000, 0, false, 10000}, {"over", 20000, 0, true, 10001}, {"cumulative", 6000, 6000, true, 10001}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(emptyInput(), `"users":[]`, `"users":[`+duplicateRows(tc.users)+`]`, 1)
			if tc.legal > 0 {
				raw = strings.Replace(raw, `"legal_entities":[]`, `"legal_entities":[`+duplicateRows(tc.legal)+`]`, 1)
			}
			r := Evaluate(context.Background(), []byte(raw))
			if r.ExitCode() != 1 || r.ChecksComplete || hasCode(r, "ROW_LIMIT") != tc.limit || r.ErrorsTotal != tc.total {
				t.Fatalf("combined limit ignored: total=%d, issues=%+v", r.ErrorsTotal, r.Issues)
			}
			if !hasCode(r, "DUPLICATE_JSON_KEY") {
				t.Fatal("prior diagnostic lost")
			}
			if tc.limit && r.Issues[0].Code != "ROW_LIMIT" {
				t.Fatal("document resource error not retained first")
			}
		})
	}
}
