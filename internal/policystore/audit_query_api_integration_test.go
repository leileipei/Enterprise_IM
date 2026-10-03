package policystore_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func assertProductionAuditQuery(t *testing.T, conn *pgx.Conn, client *http.Client, first, second, token string) {
	t.Helper()
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ('00000000-0000-4000-8000-000000009a12',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	// An actual stale-version write produces a denial audit without changing v2.
	productionResponse(t, client, productionRequest(t, "PUT", first, "/api/v1/admin/retention-policy", token, []byte(`{"message_body_days":100,"expected_version":0,"approval_reference":"CAB-AUDIT-CONFLICT"}`), true), 409, nil)
	type event struct {
		ID           string    `json:"id"`
		User         string    `json:"actor_user_id"`
		Member       string    `json:"acting_membership_id"`
		Action       string    `json:"action"`
		ResourceType string    `json:"resource_type"`
		ResourceID   *string   `json:"resource_id"`
		Outcome      string    `json:"outcome"`
		Reason       string    `json:"reason"`
		At           time.Time `json:"occurred_at"`
	}
	type page struct {
		Events []event `json:"events"`
		Next   string  `json:"next_cursor"`
	}
	const route = "/api/v1/admin/audit-events"
	const filter = "?action=retention_policy_update&outcome=allow&limit=1"
	var a, b, deny page
	productionResponse(t, client, productionRequest(t, "GET", first, route+filter, token, nil, true), 200, &a)
	if len(a.Events) != 1 || a.Next == "" || a.Events[0].ID == "" || a.Events[0].User != adminA || a.Events[0].Member != adminM || a.Events[0].Action != "retention_policy_update" || a.Events[0].ResourceType != "tenant" || a.Events[0].ResourceID == nil || *a.Events[0].ResourceID != tenantA || a.Events[0].Outcome != "allow" || a.Events[0].Reason != "approved_retention_change" || a.Events[0].At.IsZero() {
		t.Fatalf("first production audit %+v", a)
	}
	productionResponse(t, client, productionRequest(t, "GET", second, route+filter+"&cursor="+a.Next, token, nil, true), 200, &b)
	if len(b.Events) != 1 || b.Events[0].ID == a.Events[0].ID || b.Events[0].At.After(a.Events[0].At) || b.Events[0].Reason != "approved_retention_change" {
		t.Fatalf("second production audit %+v", b)
	}
	productionResponse(t, client, productionRequest(t, "GET", second, route+"?action=retention_policy_update&outcome=deny", token, nil, true), 200, &deny)
	if len(deny.Events) == 0 || deny.Events[0].Reason != "version_conflict" || deny.Events[0].Outcome != "deny" {
		t.Fatalf("production denial %+v", deny)
	}
	productionResponse(t, client, productionRequest(t, "GET", second, route+"?action=retention_policy_update&outcome=deny&cursor="+a.Next, token, nil, true), 400, nil)
	for _, addr := range []string{first, second} {
		productionResponse(t, client, productionRequest(t, "GET", addr, route, "invalid", nil, true), 401, nil)
	}
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action='audit_events_list' AND outcome='allow'`, tenantA).Scan(&n); err != nil || n != 3 {
		t.Fatalf("query audit %d %v", n, err)
	}
	run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE id='00000000-0000-4000-8000-000000009a12'`)
	for _, addr := range []string{first, second} {
		productionResponse(t, client, productionRequest(t, "GET", addr, route, token, nil, true), 404, nil)
	}
}
