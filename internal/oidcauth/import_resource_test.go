package oidcauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	a "github.com/leileipei/Enterprise_IM/internal/importapply"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type appendResourceAuth struct {
	expiry time.Time
	delay  time.Duration
}

func (s appendResourceAuth) Authenticate(ctx context.Context, _ string) (httpserver.VerifiedIdentity, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return httpserver.VerifiedIdentity{}, ctx.Err()
		}
	}
	expiry := s.expiry
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}
	return httpserver.VerifiedIdentity{TenantID: fixtureTenant, UserID: fixtureActor, Issuer: "https://sso.test", Subject: "actor", ExpiresAt: expiry}, nil
}
func resourceDocument(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var d map[string]any
	if e := json.Unmarshal(raw, &d); e != nil {
		t.Fatal(e)
	}
	return d
}
func encodeResource(t *testing.T, d map[string]any) []byte {
	t.Helper()
	b, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func resourceCall(t *testing.T, server *httptest.Server, method, id string, body []byte) (int, []byte) {
	t.Helper()
	r, e := http.NewRequest(method, server.URL+"/api/admin/import-batches/"+id, bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer test-verified")
	r.Header.Set("X-Acting-Membership-ID", fixtureMembership)
	r.Header.Set("Content-Type", "application/json")
	response, e := server.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	b, e := io.ReadAll(response.Body)
	if e != nil {
		t.Fatal(e)
	}
	if response.Header.Get("Cache-Control") != "no-store" || len(b) > p.MaxReport {
		t.Fatal("unsafe response boundary")
	}
	return response.StatusCode, b
}
func resourceServer(t *testing.T, f *appendFixture, auth appendResourceAuth) *httptest.Server {
	t.Helper()
	s, e := a.NewService(f.Pool, f.Schema)
	if e != nil {
		t.Fatal(e)
	}
	h, e := httpserver.HandlerWithImports(http.NotFoundHandler(), auth, s)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return server
}
func TestAppendHTTPResourceEdges(t *testing.T) {
	for _, scenario := range []string{"body10m", "body10mplus1", "rows10000", "rows10001", "stored20000", "stored20001", "stored64m", "stored64mplus1", "string4096", "string4097", "report200", "report201", "deepTree", "slowBodyExpiry", "disconnectAfterCommit"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			seedApplyActor(t, f)
			ctx := context.Background()
			server := resourceServer(t, f, appendResourceAuth{})
			raw := applyInput(t)
			want := 201
			d := resourceDocument(t, raw)
			tables := d["tables"].(map[string]any)
			if strings.HasPrefix(scenario, "body10m") {
				size := p.MaxInput
				if scenario == "body10mplus1" {
					size++
					want = 413
				}
				raw = append(raw, bytes.Repeat([]byte(" "), size-len(raw))...)
			}
			if strings.HasPrefix(scenario, "rows1000") {
				for name := range tables {
					if name != "tenants" {
						tables[name] = []any{}
					}
				}
				n := 9999
				if scenario == "rows10001" {
					n++
					want = 422
				}
				rows := []any{}
				for i := 1; i <= n; i++ {
					rows = append(rows, map[string]any{"id": fmt.Sprintf("99300000-0000-4000-8000-%012d", i), "tenant_id": fixtureTenant, "global_employee_no": fmt.Sprintf("r%d", i), "display_name": "Row"})
				}
				tables["users"] = rows
				raw = encodeResource(t, d)
			}
			if strings.HasPrefix(scenario, "stored") {
				for name := range tables {
					if name != "tenants" {
						tables[name] = []any{}
					}
				}
				raw = encodeResource(t, d)
				n, display := 19993, 1
				if scenario == "stored20001" {
					n++
					want = 503
				}
				if strings.HasPrefix(scenario, "stored64m") {
					baseline := storedByteCount(t, f)
					n = (64*1024*1024 - baseline - 114) / 4210
					display = 4096
					if scenario == "stored64mplus1" {
						want = 503
					}
					if _, e := f.Admin.Exec(ctx, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) SELECT ('99400000-0000-4000-8000-'||lpad(i::text,12,'0'))::uuid,$1,('99400000-0000-4000-8000-'||lpad(i::text,12,'0')),repeat('x',$3) FROM generate_series(1,$2) i", fixtureTenant, n, display); e != nil {
						t.Fatal(e)
					}
					tail := 64*1024*1024 - baseline - n*4210 - 114
					if want == 503 {
						tail++
					}
					if _, e := f.Admin.Exec(ctx, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('99500000-0000-4000-8000-000000000001',$1,'99500000-0000-4000-8000-000000000001',repeat('x',$2))", fixtureTenant, tail); e != nil {
						t.Fatal(e)
					}
				} else {
					if _, e := f.Admin.Exec(ctx, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) SELECT ('99400000-0000-4000-8000-'||lpad(i::text,12,'0'))::uuid,$1,i::text,repeat('x',$3) FROM generate_series(1,$2) i", fixtureTenant, n, display); e != nil {
						t.Fatal(e)
					}
				}
			}
			if strings.HasPrefix(scenario, "string409") {
				n := 4096
				if scenario == "string4097" {
					n++
					want = 422
				}
				tables["users"].([]any)[0].(map[string]any)["display_name"] = strings.Repeat("x", n)
				raw = encodeResource(t, d)
			}
			if strings.HasPrefix(scenario, "report20") {
				n := 200
				if scenario == "report201" {
					n++
				}
				want = 409
				for name := range tables {
					if name != "tenants" {
						tables[name] = []any{}
					}
				}
				rows := []any{}
				for i := 1; i <= n; i++ {
					id := fmt.Sprintf("99600000-0000-4000-8000-%012d", i)
					if _, e := f.Admin.Exec(ctx, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES($1,$2,$3,'Stored')", id, fixtureTenant, fmt.Sprintf("report%d", i)); e != nil {
						t.Fatal(e)
					}
					rows = append(rows, map[string]any{"id": id, "tenant_id": fixtureTenant, "global_employee_no": fmt.Sprintf("report%d", i), "display_name": "Changed"})
				}
				tables["users"] = rows
				raw = encodeResource(t, d)
			}
			if scenario == "deepTree" {
				for name := range tables {
					if name != "tenants" {
						tables[name] = []any{}
					}
				}
				tables["legal_entities"] = []any{map[string]any{"id": "95000000-0000-4000-8000-000000000010", "tenant_id": fixtureTenant, "code": "actorlegal", "name": "Actor legal"}}
				rows := []any{}
				for i := 1; i <= 300; i++ {
					var parent any
					if i > 1 {
						parent = fmt.Sprintf("99700000-0000-4000-8000-%012d", i-1)
					}
					rows = append(rows, map[string]any{"id": fmt.Sprintf("99700000-0000-4000-8000-%012d", i), "tenant_id": fixtureTenant, "parent_id": parent, "legal_entity_id": "95000000-0000-4000-8000-000000000010", "org_type": "company", "code": fmt.Sprintf("deep%d", i), "name": "Deep"})
				}
				tables["organizations"] = rows
				raw = encodeResource(t, d)
			}
			if scenario == "slowBodyExpiry" {
				server.Close()
				start := time.Now()
				server = resourceServer(t, f, appendResourceAuth{expiry: start.Add(600 * time.Millisecond), delay: 150 * time.Millisecond})
				u, _ := url.Parse(server.URL)
				conn, e := net.Dial("tcp", u.Host)
				if e != nil {
					t.Fatal(e)
				}
				defer conn.Close()
				conn.SetDeadline(start.Add(2 * time.Second))
				fmt.Fprintf(conn, "POST /api/admin/import-batches/%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer test\r\nX-Acting-Membership-ID: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", fixtureRequest, u.Host, fixtureMembership)
				b := make([]byte, 1024)
				n, e := conn.Read(b)
				if e != nil || !bytes.Contains(b[:n], []byte("503")) || time.Since(start) > 1200*time.Millisecond {
					t.Fatal("body stage reset or ignored absolute expiry", e)
				}
				var count int
				if e = f.Pool.QueryRow(ctx, "SELECT count(*) FROM import_batches").Scan(&count); e != nil || count != 0 {
					t.Fatal("slow body persisted")
				}
				return
			}
			if scenario == "disconnectAfterCommit" {
				server.Close()
				s, _ := a.NewService(f.Pool, f.Schema)
				h, _ := httpserver.HandlerWithImports(http.NotFoundHandler(), appendResourceAuth{}, s)
				var dropped atomic.Bool
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					h.ServeHTTP(&dropCommitResponse{ResponseWriter: w, drop: !dropped.Swap(true)}, r)
				}))
				defer server.Close()
				r, _ := http.NewRequest("POST", server.URL+"/api/admin/import-batches/"+fixtureRequest, bytes.NewReader(raw))
				r.Header.Set("Authorization", "Bearer test")
				r.Header.Set("X-Acting-Membership-ID", fixtureMembership)
				r.Header.Set("Content-Type", "application/json")
				response, requestErr := server.Client().Do(r)
				if requestErr == nil {
					body, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr == nil {
						if _, decodeErr := a.DecodeReceipt(body); decodeErr == nil {
							t.Fatal("disconnect delivered complete receipt")
						}
					}
				}
				t.Log("HTTP_connection_closed_after_commit_before_receipt_body")

				status, b := resourceCall(t, server, "POST", fixtureRequest, raw)
				if status != 200 {
					t.Fatal("post-commit disconnect not replayed", status)
				}
				if _, e := a.DecodeReceipt(b); e != nil {
					t.Fatal(e)
				}
				var count int
				f.Pool.QueryRow(ctx, "SELECT count(*) FROM import_batches").Scan(&count)
				if count != 1 {
					t.Fatal("disconnect duplicated batch")
				}
				return
			}
			status, b := resourceCall(t, server, "POST", fixtureRequest, raw)
			if status != want {
				t.Fatalf("status=%d want=%d response=%s", status, want, b)
			}
			if status == 201 || status == 409 {
				receipt, e := a.DecodeReceipt(b)
				if e != nil {
					t.Fatal(e)
				}
				if strings.HasPrefix(scenario, "report") {
					n := 200
					if scenario == "report201" {
						n++
					}
					if receipt.ErrorsTotal != n || len(receipt.Issues) != 200 || receipt.IssuesTruncated != (n > 200) || receipt.Counts["total"].Inserted != 0 {
						t.Fatal("report cap changed counts")
					}
				}
				getStatus, get := resourceCall(t, server, "GET", fixtureRequest, nil)
				if getStatus != 200 || !bytes.Equal(get, b) {
					t.Fatal("pooled GET differs")
				}
			} else {
				var count int
				f.Pool.QueryRow(ctx, "SELECT count(*) FROM import_batches").Scan(&count)
				if count != 0 {
					t.Fatal("failed boundary left terminal")
				}
			}
		})
	}
}

type dropCommitResponse struct {
	http.ResponseWriter
	drop bool
}

func (w *dropCommitResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *dropCommitResponse) Write(b []byte) (int, error) {
	if w.drop {
		conn, _, e := http.NewResponseController(w.ResponseWriter).Hijack()
		if e != nil {
			return 0, e
		}
		conn.Close()
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(b)
}
func storedByteCount(t *testing.T, f *appendFixture) int {
	t.Helper()
	total := 0
	for _, s := range p.Schema() {
		cells := []string{}
		for _, field := range s.Fields {
			cell := pgx.Identifier{string(field.Name)}.Sanitize() + "::text"
			if field.Kind == "time" {
				cell = "pg_catalog.to_char(" + pgx.Identifier{string(field.Name)}.Sanitize() + " AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')"
			}
			cells = append(cells, "coalesce(pg_catalog.octet_length("+cell+"),0)")
		}
		var n int
		if e := f.Admin.QueryRow(context.Background(), "SELECT coalesce(sum("+strings.Join(cells, "+")+"),0) FROM "+pgx.Identifier{f.Schema, string(s.Entity)}.Sanitize()).Scan(&n); e != nil {
			t.Fatal(e)
		}
		total += n
	}
	return total
}
