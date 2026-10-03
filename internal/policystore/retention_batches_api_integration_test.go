package policystore_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runs against both production im-api processes using real OIDC and PostgreSQL.
func assertProductionRetentionBatches(t *testing.T, conn *pgx.Conn, client *http.Client, first, second, token string) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	run(t, conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-000000009a01',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	run(t, conn, `INSERT INTO message_body_clear_batches(id,tenant_id,conversation_id,retention_days,cutoff_at,cleared_at,first_seq,last_seq,cleared_count)
 VALUES ('00000000-0000-4000-8000-000000009b01',$1,$2,365,$3::timestamptz-INTERVAL '8760 hours',$3,1,3,2)`, tenantA, directA, at)
	run(t, conn, `INSERT INTO message_digest_retirement_batches(id,tenant_id,conversation_id,retired_at,retired_count,first_seq,last_seq,min_expires_at,max_expires_at)
 VALUES ('00000000-0000-4000-8000-000000009d01',$1,$2,$3,2,1,3,$3,$3)`, tenantA, directA, at)
	for _, addr := range []string{first, second} {
		for _, kind := range []string{"body", "digest"} {
			path := "/api/v1/admin/conversations/" + directA + "/retention-batches?kind=" + kind
			productionResponse(t, client, productionRequest(t, http.MethodGet, addr, path, "invalid", nil, true), 401, nil)
			var page struct {
				Batches []struct {
					Kind  string    `json:"kind"`
					Count int       `json:"processed_count"`
					At    time.Time `json:"processed_at"`
				} `json:"batches"`
				Cursor string `json:"next_cursor"`
			}
			productionResponse(t, client, productionRequest(t, http.MethodGet, addr, path, token, nil, true), 200, &page)
			if len(page.Batches) != 1 || page.Batches[0].Kind != kind || page.Batches[0].Count != 2 || !page.Batches[0].At.Equal(at) || page.Cursor != "" {
				t.Fatalf("production evidence: %+v", page)
			}
		}
	}
	run(t, conn, `UPDATE admin_grants SET status='revoked' WHERE id='00000000-0000-4000-8000-000000009a01'`)
	productionResponse(t, client, productionRequest(t, http.MethodGet, first, "/api/v1/admin/conversations/"+directA+"/retention-batches?kind=body", token, nil, true), 404, nil)
}
