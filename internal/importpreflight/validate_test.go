package importpreflight

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

type mutationCase struct {
	id     string
	entity Entity
	row    int
	field  Field
	code   Code
	state  string
	mutate func(map[string]any)
}

func mutations() []mutationCase {
	appendRow := func(m map[string]any, e string, i int, changes map[string]any) {
		rows := tableRows(m, e)
		r := map[string]any{}
		for k, v := range rows[i].(map[string]any) {
			r[k] = v
		}
		for k, v := range changes {
			r[k] = v
		}
		m["tables"].(map[string]any)[e] = append(rows, r)
	}
	row := func(m map[string]any, e string, i int) map[string]any { return tableRows(m, e)[i].(map[string]any) }
	return []mutationCase{
		{"DB01", "external_identities", 12, "subject", "PK_DUPLICATE", "23505", func(m map[string]any) {
			appendRow(m, "external_identities", 2, map[string]any{"user_id": row(m, "users", 3)["id"]})
		}},
		{"DB02", "external_identities", 12, "issuer", "UNIQUE_DUPLICATE", "23505", func(m map[string]any) {
			appendRow(m, "external_identities", 2, map[string]any{"subject": "synthetic-subject-03-alias"})
		}},
		{"DB03", "external_identities", 12, "user_id", "REF_SCOPE_MISMATCH", "23503", func(m map[string]any) {
			appendRow(m, "external_identities", 2, map[string]any{"subject": "synthetic-cross-tenant", "tenant_id": row(m, "tenants", 1)["id"]})
		}},
		{"DB04", "organizations", 1, "parent_id", "TREE_CYCLE", "23514", func(m map[string]any) { row(m, "organizations", 0)["parent_id"] = row(m, "organizations", 1)["id"] }},
		{"DB05", "departments", 3, "parent_id", "REF_SCOPE_MISMATCH", "23503", func(m map[string]any) { row(m, "departments", 2)["parent_id"] = row(m, "departments", 3)["id"] }},
		{"DB06", "user_organizations", 16, "organization_id", "VIRTUAL_MEMBERSHIP", "23514", func(m map[string]any) {
			appendRow(m, "user_organizations", 0, map[string]any{"id": "c43c5b61-67db-5a9d-b8d8-64579153b09f", "organization_id": row(m, "organizations", 0)["id"], "is_primary": false})
		}},
		{"DB07", "user_organizations", 16, "effective_from", "INTERVAL_OVERLAP", "23P01", func(m map[string]any) {
			appendRow(m, "user_organizations", 2, map[string]any{"id": "2b42f51f-fa61-5b13-917e-7cdff2e37cd7", "is_primary": false})
		}},
		{"DB08", "user_organizations", 16, "effective_from", "PRIMARY_OVERLAP", "23P01", func(m map[string]any) {
			appendRow(m, "user_organizations", 0, map[string]any{"id": "61cef58b-ba0c-52f1-9b28-c05a85074221", "organization_id": row(m, "organizations", 3)["id"]})
		}},
		{"DB09", "user_departments", 16, "effective_from", "DEPARTMENT_INTERVAL_OUTSIDE", "23514", func(m map[string]any) {
			appendRow(m, "user_departments", 12, map[string]any{"id": "98c4455b-c19f-5794-b4fb-e736755d9bf4", "effective_to": nil, "is_primary": false})
		}},
		{"DB10", "user_departments", 3, "effective_from", "DEPARTMENT_INTERVAL_OUTSIDE", "23514", func(m map[string]any) { row(m, "user_organizations", 2)["effective_to"] = "2026-10-01T00:00:00+08:00" }},
		{"DB11", "users", 13, "global_employee_no", "UNIQUE_DUPLICATE", "23505", func(m map[string]any) {
			appendRow(m, "users", 2, map[string]any{"id": "3dfae135-8cb7-5ffd-a65e-15049c1d1f44"})
		}},
	}
}
func TestEvaluateSampleAndElevenMutations(t *testing.T) {
	r := Evaluate(context.Background(), sampleBytes(t))
	if r.ExitCode() != 0 || !r.ChecksComplete || *r.Counts["total"] != 74 {
		t.Fatalf("sample: %+v", r)
	}
	for _, tc := range mutations() {
		t.Run(tc.id, func(t *testing.T) {
			r := Evaluate(context.Background(), mutateBytes(t, sampleBytes(t), tc.mutate))
			if r.ExitCode() != 1 || !r.ChecksComplete {
				t.Fatalf("mutation: %+v", r)
			}
			found := false
			for _, i := range r.Issues {
				if i.Entity == tc.entity && i.Row == tc.row && i.Field == tc.field && i.Code == tc.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing exact position: %+v", r.Issues)
			}
		})
	}
	if r := Evaluate(context.Background(), []byte(emptyInput())); r.ExitCode() != 0 {
		t.Fatal("empty tables rejected")
	}
}
func TestEvaluateHashAndCancellation(t *testing.T) {
	b := sampleBytes(t)
	r := Evaluate(context.Background(), b)
	want := fmt.Sprintf("%x", sha256.Sum256(b))
	if r.InputSHA256 == nil || *r.InputSHA256 != want {
		t.Fatal("hash")
	}
	encoded, _ := EncodeReport(r)
	for i := 0; i < 5; i++ {
		v, _ := EncodeReport(Evaluate(context.Background(), b))
		if !bytes.Equal(v, encoded) {
			t.Fatal("nondeterministic")
		}
	}
	r2 := Evaluate(context.Background(), append(b, ' '))
	if r2.ExitCode() != 0 || *r2.InputSHA256 == want {
		t.Fatal("raw bytes hash")
	}
	for _, timeout := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if timeout {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		cancel()
		r = Evaluate(ctx, b)
		code := Code("CANCELED")
		if timeout {
			code = "TIMEOUT"
		}
		if r.ExitCode() != 2 || r.ChecksComplete || !hasCode(r, code) {
			t.Fatal("canceled success")
		}
	}
}
