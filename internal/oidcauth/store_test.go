package oidcauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"crypto/rand"
	"crypto/rsa"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const (
	storeTenantA = "00000000-0000-4000-8000-000000000701"
	storeTenantB = "00000000-0000-4000-8000-000000000702"
	storeUserA   = "00000000-0000-4000-8000-000000000711"
	storeUserB   = "00000000-0000-4000-8000-000000000712"
)

func identityDB(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IM_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("im_oidc_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); conn.Close(ctx) })
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../../db/migrations/000001_group_foundation.up.sql", "../../db/migrations/000002_admin_access.up.sql", "../../db/migrations/000003_policy_store.up.sql", "../../db/migrations/000004_external_identities.up.sql"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(ctx, string(data)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func seedIdentities(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	_, err := conn.Exec(ctx, "INSERT INTO tenants (id,code,name) VALUES ($1,'a','Group A'),($2,'b','Group B')", storeTenantA, storeTenantB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A1','User A'),($3,$4,'B1','User B')", storeUserA, storeTenantA, storeUserB, storeTenantB)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExternalIdentityBindingAndLookup(t *testing.T) {
	conn := identityDB(t)
	seedIdentities(t, conn)
	ctx := context.Background()
	store := Store{DB: conn}
	_, err := conn.Exec(ctx, "INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,$2,$3,$4)", testIssuer, testSubject, storeTenantA, storeUserA)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Lookup(ctx, testIssuer, testSubject)
	if err != nil || id.TenantID != storeTenantA || id.UserID != storeUserA {
		t.Fatalf("lookup=%+v err=%v", id, err)
	}
	for _, args := range [][]any{
		{testIssuer, testSubject, storeTenantB, storeUserB},
		{testIssuer, "different-subject", storeTenantA, storeUserB},
		{testIssuer, "second-subject", storeTenantA, storeUserA},
	} {
		if _, err := conn.Exec(ctx, "INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,$2,$3,$4)", args...); err == nil {
			t.Fatalf("unsafe binding accepted: %v", args)
		}
	}
	if _, err := conn.Exec(ctx, "INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ('https://other.example.test','other-sub',$1,$2)", storeTenantA, storeUserA); err != nil {
		t.Fatalf("multiple sources should map to one user: %v", err)
	}
	if _, err := store.Lookup(ctx, testIssuer, "not-bound"); !errors.Is(err, ErrIdentityNotFound) {
		t.Fatalf("missing binding: %v", err)
	}
}

func TestInactiveBindingTenantOrUserCannotAuthenticate(t *testing.T) {
	conn := identityDB(t)
	seedIdentities(t, conn)
	ctx := context.Background()
	store := Store{DB: conn}
	if _, err := conn.Exec(ctx, "INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,$2,$3,$4)", testIssuer, testSubject, storeTenantA, storeUserA); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE external_identities SET status='disabled' WHERE issuer=$1 AND subject=$2", []any{testIssuer, testSubject}},
		{"UPDATE users SET status='frozen' WHERE id=$1", []any{storeUserA}},
		{"UPDATE tenants SET status='suspended' WHERE id=$1", []any{storeTenantA}},
	} {
		if _, err := conn.Exec(ctx, tc.sql, tc.args...); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Lookup(ctx, testIssuer, testSubject); !errors.Is(err, ErrIdentityNotFound) {
			t.Fatalf("inactive identity still available after %s: %v", tc.sql, err)
		}
		if _, err := conn.Exec(ctx, "UPDATE external_identities SET status='active' WHERE issuer=$1 AND subject=$2", testIssuer, testSubject); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "UPDATE users SET status='active' WHERE id=$1", storeUserA); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "UPDATE tenants SET status='active' WHERE id=$1", storeTenantA); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExternalIdentityMigrationRollsBackAndReapplies(t *testing.T) {
	conn := identityDB(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000004_external_identities.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var name *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('external_identities')::text").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != nil {
		t.Fatalf("table remains after rollback: %s", *name)
	}
	up, err := os.ReadFile("../../db/migrations/000004_external_identities.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	seedIdentities(t, conn)
}

func TestSignedTokenThroughHTTPToAuditedAdminAndOrdinaryDirectory(t *testing.T) {
	conn := identityDB(t)
	seedIdentities(t, conn)
	ctx := context.Background()
	targetID := "00000000-0000-4000-8000-000000000713"
	legalID := "00000000-0000-4000-8000-000000000721"
	orgID := "00000000-0000-4000-8000-000000000731"
	actorMembership := "00000000-0000-4000-8000-000000000741"
	targetMembership := "00000000-0000-4000-8000-000000000742"
	grantID := "00000000-0000-4000-8000-000000000751"
	statements := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A2','Target User')", []any{targetID, storeTenantA}},
		{"INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'legal','Legal A')", []any{legalID, storeTenantA}},
		{"INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','org','Org A')", []any{orgID, storeTenantA, legalID}},
		{"INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01'),($5,$2,$6,$4,'2020-01-01')", []any{actorMembership, storeTenantA, storeUserA, orgID, targetMembership, targetID}},
		{"INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ($1,$2,$3,$4,'group_admin','2020-01-01')", []any{grantID, storeTenantA, actorMembership, orgID}},
		{"INSERT INTO external_identities (issuer,subject,tenant_id,user_id) VALUES ($1,$2,$3,$4)", []any{testIssuer, testSubject, storeTenantA, storeUserA}},
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthenticator(config(), func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, Store{DB: conn})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpserver.HandlerWithAdmin(conn, auth, access.Service{DB: conn})
	if err != nil {
		t.Fatal(err)
	}
	handler, err = httpserver.HandlerWithDirectory(handler, auth, policystore.Service{DB: conn})
	if err != nil {
		t.Fatal(err)
	}
	accessToken := signToken(t, key, testClaims(), "at+jwt", jwt.SigningMethodRS256)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users/"+targetID, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Acting-Membership-ID", actorMembership)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"display_name":"Target User"`) {
		t.Fatalf("admin response: %d %s", res.Code, res.Body.String())
	}
	var auditCount int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='directory_view' AND outcome='allow' AND actor_user_id=$1", storeUserA).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("expected one allowed audit event, got %d", auditCount)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/directory/memberships/"+targetMembership, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Acting-Membership-ID", actorMembership)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"display_name":"Target User"`) {
		t.Fatalf("ordinary directory response: %d %s", res.Code, res.Body.String())
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM policy_decision_events WHERE action='directory_view' AND allowed AND target_membership_id=$1", targetMembership).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("ordinary directory audit: %d %v", auditCount, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/directory/users?employee_no=A2", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Acting-Membership-ID", actorMembership)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"employee_no":"A2"`) ||
		!strings.Contains(res.Body.String(), `"membership_id":"`+targetMembership+`"`) {
		t.Fatalf("ordinary lookup response: %d %s", res.Code, res.Body.String())
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='directory_lookup' AND outcome='allow' AND actor_user_id=$1", storeUserA).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("ordinary lookup audit: %d %v", auditCount, err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/directory/users?q=Target&limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Acting-Membership-ID", actorMembership)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"people":[`) ||
		!strings.Contains(res.Body.String(), `"employee_no":"A2"`) ||
		!strings.Contains(res.Body.String(), `"membership_id":"`+targetMembership+`"`) {
		t.Fatalf("ordinary name search response: %d %s", res.Code, res.Body.String())
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='directory_name_search' AND outcome='allow' AND actor_user_id=$1", storeUserA).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("ordinary name search audit: %d %v", auditCount, err)
	}
	orgReq := httptest.NewRequest(http.MethodGet, "/api/v1/directory/organizations", nil)
	orgReq.Header.Set("Authorization", "Bearer "+accessToken)
	orgReq.Header.Set("X-Acting-Membership-ID", actorMembership)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, orgReq)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"organizations":[`) ||
		!strings.Contains(res.Body.String(), `"id":"`+orgID+`"`) ||
		!strings.Contains(res.Body.String(), `"has_visible_members":true`) {
		t.Fatalf("ordinary organization response: %d %s", res.Code, res.Body.String())
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='directory_organization_list' AND outcome='allow' AND actor_user_id=$1", storeUserA).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("ordinary organization audit: %d %v", auditCount, err)
	}
	if _, err := conn.Exec(ctx, "UPDATE user_organizations SET status='ended' WHERE id=$1", actorMembership); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/directory/users?employee_no=A2", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Acting-Membership-ID", actorMembership)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("ended acting membership: %d %s", res.Code, res.Body.String())
	}
	searchReq := httptest.NewRequest(http.MethodGet, "/api/v1/directory/users?q=Target", nil)
	searchReq.Header.Set("Authorization", "Bearer "+accessToken)
	searchReq.Header.Set("X-Acting-Membership-ID", actorMembership)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, searchReq)
	if res.Code != http.StatusForbidden {
		t.Fatalf("ended acting membership name search: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, orgReq)
	if res.Code != http.StatusForbidden {
		t.Fatalf("ended acting membership organization list: %d %s", res.Code, res.Body.String())
	}
	if _, err := conn.Exec(ctx, "UPDATE users SET status='frozen' WHERE id=$1", storeUserA); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("frozen account authentication: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, searchReq)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("frozen account name search authentication: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, orgReq)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("frozen account organization authentication: %d %s", res.Code, res.Body.String())
	}
}
