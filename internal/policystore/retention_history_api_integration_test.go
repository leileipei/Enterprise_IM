package policystore_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Verifies production route wiring and version cursor portability across nodes.
func assertProductionRetentionHistory(t *testing.T, conn *pgx.Conn, client *http.Client, first, second, token string) {
	t.Helper()
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-000000009a11',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	for _, update := range []string{`{"message_body_days":300,"expected_version":0,"approval_reference":"CAB-PROD-HISTORY-1"}`, `{"message_body_days":200,"expected_version":1,"approval_reference":"CAB-PROD-HISTORY-2"}`} {
		productionResponse(t, client, productionRequest(t, "PUT", first, "/api/v1/admin/retention-policy", token, []byte(update), true), 200, nil)
	}
	type approval struct {
		Version   int64      `json:"version"`
		Days      int        `json:"message_body_days"`
		Reference string     `json:"approval_reference"`
		UserID    string     `json:"approved_by_user_id"`
		At        *time.Time `json:"approved_at"`
	}
	type page struct {
		History []approval `json:"history"`
		Cursor  string     `json:"next_cursor"`
	}
	const route = "/api/v1/admin/retention-policy/history"
	var a, b page
	productionResponse(t, client, productionRequest(t, "GET", first, route+"?limit=1", token, nil, true), 200, &a)
	if len(a.History) != 1 || a.History[0].Version != 2 || a.History[0].Days != 200 || a.History[0].Reference != "CAB-PROD-HISTORY-2" || a.History[0].UserID != adminA || a.History[0].At == nil || a.Cursor == "" {
		t.Fatalf("first production history %+v", a)
	}
	productionResponse(t, client, productionRequest(t, "GET", second, route+"?limit=1&cursor="+a.Cursor, token, nil, true), 200, &b)
	if len(b.History) != 1 || b.History[0].Version != 1 || b.History[0].Days != 300 || b.History[0].Reference != "CAB-PROD-HISTORY-1" || b.Cursor != "" {
		t.Fatalf("second production history %+v", b)
	}
	for _, addr := range []string{first, second} {
		productionResponse(t, client, productionRequest(t, "GET", addr, route, "invalid", nil, true), 401, nil)
	}
	var reads int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action='retention_policy_history_list' AND outcome='allow'`, tenantA).Scan(&reads); err != nil || reads != 2 {
		t.Fatalf("production history audits %d %v", reads, err)
	}
	run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE id='00000000-0000-4000-8000-000000009a11'`)
	for _, addr := range []string{first, second} {
		productionResponse(t, client, productionRequest(t, "GET", addr, route, token, nil, true), 404, nil)
	}
}
