package policystore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

func fileRuntimeMigration(t *testing.T, c *pgx.Conn, direction string) error {
	t.Helper()
	b, e := os.ReadFile("../../db/migrations/000019_file_upload_scan." + direction + ".sql")
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll()
	return e
}
func runtimeAttempt(t *testing.T, c *pgx.Conn, m files.Metadata) string {
	t.Helper()
	id := freshFile().ID
	run(t, c, `INSERT INTO file_upload_attempts(id,tenant_id,file_id,phase,lease_token,owner_id,lease_expires_at,created_at,updated_at)
 VALUES($1,$2,$3,'receiving',$4,$5,$6,$7,$7)`, id, m.TenantID, m.ID, freshFile().ID, freshFile().ID, m.CreatedAt.Add(180*time.Second), m.CreatedAt)
	return id
}
func runtimePolicyEdit(t *testing.T, c *pgx.Conn) {
	t.Helper()
	run(t, c, `UPDATE tenant_file_upload_policy SET enabled=true,version=1,approval_reference='approval-1',actor_user_id=$2,acting_membership_id=$3,updated_at=clock_timestamp() WHERE tenant_id=$1`, tenantA, adminA, adminM)
}
func runtimeHistory(t *testing.T, c *pgx.Conn) {
	t.Helper()
	run(t, c, `INSERT INTO tenant_file_upload_policy_history(tenant_id,enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version,approval_reference,actor_user_id,acting_membership_id,updated_at)
 SELECT tenant_id,enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version,approval_reference,actor_user_id,acting_membership_id,updated_at FROM tenant_file_upload_policy WHERE tenant_id=$1`, tenantA)
}
func runtimeAudit(t *testing.T, c *pgx.Conn, m files.Metadata) {
	t.Helper()
	run(t, c, `INSERT INTO file_worker_audit_events(tenant_id,file_id,worker_id,job_id,operation,reason_code,outcome,occurred_at)
 VALUES($1,$2,$3,$4,'upload_recovery_claim','recovery_started','allow',$5)`, m.TenantID, m.ID, freshFile().ID, freshFile().ID, m.CreatedAt)
}
func runtimeScanJob(t *testing.T, c *pgx.Conn, m files.Metadata) {
	t.Helper()
	run(t, c, `INSERT INTO file_scan_jobs(id,tenant_id,file_id,claim_version,sealed_sha256,attempt,status,lease_token,owner_id,lease_expires_at,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,1,'running',$6,$7,$8,$9,$9)`, m.ScanJobID, m.TenantID, m.ID, m.StateVersion, m.SHA256, freshFile().ID, freshFile().ID, m.UpdatedAt.Add(120*time.Second), m.UpdatedAt)
}
func TestFileRuntimeSchemaDefaults(t *testing.T) {
	c := db(t)
	seed(t, c)
	// Check both trigger defaults and migration backfill for tenants that predate 000019.
	if e := fileRuntimeMigration(t, c, "down"); e != nil {
		t.Fatal(e)
	}
	if e := fileRuntimeMigration(t, c, "up"); e != nil {
		t.Fatal(e)
	}
	var enabled bool
	var size, ttl, budget, version int64
	var types []string
	if e := c.QueryRow(context.Background(), `SELECT enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version FROM tenant_file_upload_policy WHERE tenant_id=$1`, tenantA).Scan(&enabled, &size, &types, &ttl, &budget, &version); e != nil {
		t.Fatal(e)
	}
	if enabled || size != 26214400 || ttl != 900 || budget != 1073741824 || version != 0 || len(types) != 4 || types[0] != "application/pdf" || types[3] != "text/plain" {
		t.Fatal(enabled, size, types, ttl, budget, version)
	}
	run(t, c, `INSERT INTO tenants(id,code,name) VALUES('00000000-0000-4000-8000-000000000299','new','新租户')`)
	var equal bool
	if e := c.QueryRow(context.Background(), `SELECT (enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version)=(false,26214400,ARRAY['application/pdf','image/jpeg','image/png','text/plain']::text[],900,1073741824,0) FROM tenant_file_upload_policy WHERE tenant_id='00000000-0000-4000-8000-000000000299'`).Scan(&equal); e != nil || !equal {
		t.Fatal(equal, e)
	}
	for _, set := range []string{"allowed_media_types=NULL", "allowed_media_types=ARRAY[NULL]::text[]", "allowed_media_types=ARRAY['application/pdf','application/pdf']", "allowed_media_types=ARRAY['application/zip']", "max_size_bytes=0", "tenant_storage_budget_bytes=1099511627777", "upload_ttl_seconds=59", "version=-1", "updated_at='infinity'"} {
		if _, e := c.Exec(context.Background(), "UPDATE tenant_file_upload_policy SET "+set+" WHERE tenant_id=$1", tenantA); e == nil {
			t.Fatal("invalid policy escaped", set)
		}
	}
}
func TestFileRuntimeSchemaAttemptConstraints(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, "allocated")
	id := runtimeAttempt(t, c, m)
	for _, set := range []string{"phase='unknown'", "phase='received'", "phase='recovered'", "object_version_id='null'", "actual_size_bytes=1", "lease_expires_at='infinity'", "lease_expires_at=created_at", "lease_expires_at=created_at+interval '2 hours'", "lookup_attempts=4", "recovery_job_id='00000000-0000-4000-8000-000000000299'"} {
		if _, e := c.Exec(context.Background(), "UPDATE file_upload_attempts SET "+set+" WHERE id=$1", id); e == nil {
			t.Fatal("invalid attempt escaped", set)
		}
	}
	_, e := c.Exec(context.Background(), `INSERT INTO file_upload_attempts(id,tenant_id,file_id,phase,lease_token,owner_id,lease_expires_at,created_at,updated_at)
 SELECT $2,tenant_id,file_id,phase,lease_token,owner_id,lease_expires_at,created_at,updated_at FROM file_upload_attempts WHERE id=$1`, id, freshFile().ID)
	expectFileSQLState(t, e, "23505")
	_, e = c.Exec(context.Background(), `INSERT INTO file_upload_attempts(id,tenant_id,file_id,phase,lease_token,owner_id,lease_expires_at,created_at,updated_at)
 SELECT $2,$3,file_id,phase,lease_token,owner_id,lease_expires_at,created_at,updated_at FROM file_upload_attempts WHERE id=$1`, id, freshFile().ID, tenantB)
	expectFileSQLState(t, e, "23503")
	run(t, c, `UPDATE file_upload_attempts SET phase='received',actual_size_bytes=1,sha256=decode(repeat('ab',32),'hex'),detected_media_type='application/pdf' WHERE id=$1`, id)
	_, e = c.Exec(context.Background(), `UPDATE file_upload_attempts SET sha256=decode(repeat('cd',32),'hex') WHERE id=$1`, id)
	expectFileSQLState(t, e, "23514")
	_, e = c.Exec(context.Background(), `DELETE FROM file_upload_attempts WHERE id=$1`, id)
	expectFileSQLState(t, e, "23514")
	var state string
	var v int64
	if e = c.QueryRow(context.Background(), `SELECT state,state_version FROM file_objects WHERE id=$1`, m.ID).Scan(&state, &v); e != nil || state != "allocated" || v != 0 {
		t.Fatal(state, v, e)
	}
}
func TestFileRuntimeSchemaImmutableEvidence(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, "allocated")
	runtimePolicyEdit(t, c)
	runtimeHistory(t, c)
	runtimeAudit(t, c, m)
	for _, q := range []string{`UPDATE tenant_file_upload_policy_history SET approval_reference='changed'`, `DELETE FROM tenant_file_upload_policy_history`, `UPDATE file_worker_audit_events SET reason_code='recovery_pending'`, `DELETE FROM file_worker_audit_events`} {
		_, e := c.Exec(context.Background(), q)
		expectFileSQLState(t, e, "23514")
	}
	_, e := c.Exec(context.Background(), `INSERT INTO file_worker_audit_events(tenant_id,file_id,worker_id,job_id,operation,reason_code,outcome,occurred_at) VALUES($1,$2,$3,$4,'invented','raw-virus-output','allow',$5)`, tenantA, m.ID, freshFile().ID, freshFile().ID, m.CreatedAt)
	expectFileSQLState(t, e, "23514")
}
func TestFileRuntimeSchemaScanHistory(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, "scanning")
	runtimeScanJob(t, c, m)
	for _, set := range []string{"sealed_sha256=NULL", "sealed_sha256=decode('ab','hex')", "attempt=4", "claim_version=0", "lease_expires_at='infinity'", "status='unknown'"} {
		if _, e := c.Exec(context.Background(), "UPDATE file_scan_jobs SET "+set+" WHERE id=$1", m.ScanJobID); e == nil {
			t.Fatal("invalid scan job escaped", set)
		}
	}
	m = fileNext(m, "scan_failed")
	if e := writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	var v int64
	if e := c.QueryRow(context.Background(), `SELECT claim_version FROM file_scan_jobs WHERE id=$1`, m.ScanJobID).Scan(&v); e != nil || v != m.StateVersion-1 {
		t.Fatal("historical claim linked to current version", v, e)
	}
}
func TestFileRuntimeDownEmptyPreservesPriorData(t *testing.T) {
	c := db(t)
	seedBodyMessage(t, c)
	m := storedFile(t, c, "allocated")
	if e := fileRuntimeMigration(t, c, "down"); e != nil {
		t.Fatal(e)
	}
	var filesCount, messagesCount int
	if e := c.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM file_objects),(SELECT count(*) FROM messages)`).Scan(&filesCount, &messagesCount); e != nil || filesCount != 1 || messagesCount != 1 {
		t.Fatal(filesCount, messagesCount, e)
	}
	if e := fileRuntimeMigration(t, c, "up"); e != nil {
		t.Fatal(e)
	}
	if e := c.QueryRow(context.Background(), `SELECT count(*) FROM file_objects WHERE id=$1`, m.ID).Scan(&filesCount); e != nil || filesCount != 1 {
		t.Fatal(e)
	}
}
func TestFileRuntimeDownDefaultTamper(t *testing.T) {
	c := db(t)
	seed(t, c)
	runtimePolicyEdit(t, c)
	e := fileRuntimeMigration(t, c, "down")
	expectFileSQLState(t, e, "23514")
	run(t, c, "ROLLBACK")
	var n int
	if e = c.QueryRow(context.Background(), `SELECT count(*) FROM tenant_file_upload_policy_history`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestFileRuntimeDownConcurrentEvidence(t *testing.T) {
	for _, kind := range []string{"attempt", "scan", "audit", "history"} {
		t.Run(kind, func(t *testing.T) {
			c := db(t)
			seedBodyMessage(t, c)
			m := storedFile(t, c, "allocated")
			if kind == "scan" {
				m = storedFile(t, c, "scanning")
			}
			if kind == "history" {
				runtimePolicyEdit(t, c)
			}
			p := filePeer(t, c)
			run(t, c, "BEGIN")
			switch kind {
			case "attempt":
				runtimeAttempt(t, c, m)
			case "scan":
				runtimeScanJob(t, c, m)
			case "audit":
				runtimeAudit(t, c, m)
			case "history":
				runtimeHistory(t, c)
				run(t, c, `UPDATE tenant_file_upload_policy SET enabled=false,version=0,approval_reference=NULL,actor_user_id=NULL,acting_membership_id=NULL WHERE tenant_id=$1`, tenantA)
			}
			b, e := os.ReadFile("../../db/migrations/000019_file_upload_scan.down.sql")
			if e != nil {
				t.Fatal(e)
			}
			result := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			go func() { _, e := p.PgConn().Exec(ctx, string(b)).ReadAll(); result <- e }()
			waitFileLock(t, c, p, "AccessExclusiveLock", func() { run(t, c, "ROLLBACK") })
			run(t, c, "COMMIT")
			e = <-result
			expectFileSQLState(t, e, "23514")
			run(t, p, "ROLLBACK")
			var n int
			if e = c.QueryRow(context.Background(), `SELECT count(*) FROM messages`).Scan(&n); e != nil || n != 1 {
				t.Fatal("old message removed", n, e)
			}
			if e = c.QueryRow(context.Background(), `SELECT count(*) FROM file_objects WHERE id=$1`, m.ID).Scan(&n); e != nil || n != 1 {
				t.Fatal("file removed", n, e)
			}
		})
	}
}

func TestFileRuntimeSchemaScanClaimBinding(t *testing.T) {
	for _, kind := range []string{"hash", "claim_version", "unsealed"} {
		t.Run(kind, func(t *testing.T) {
			c := db(t)
			seedDirectConversation(t, c)
			m := storedFile(t, c, "scanning")
			job := m.ScanJobID
			sha := m.SHA256
			v := m.StateVersion
			if kind == "hash" {
				sha = make([]byte, 32)
			}
			if kind == "claim_version" {
				v++
			}
			if kind == "unsealed" {
				m = storedFile(t, c, "allocated")
				job = freshFile().ID
			}
			_, e := c.Exec(context.Background(), `INSERT INTO file_scan_jobs(id,tenant_id,file_id,claim_version,sealed_sha256,attempt,status,lease_token,owner_id,lease_expires_at,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,1,'running',$6,$7,$8,$9,$9)`, job, m.TenantID, m.ID, v, sha, freshFile().ID, freshFile().ID, m.UpdatedAt.Add(120*time.Second), m.UpdatedAt)
			expectFileSQLState(t, e, "23514")
		})
	}
}
