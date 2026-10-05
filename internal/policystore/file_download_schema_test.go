package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

func TestFileDownloadSchemaSources(t *testing.T) {
	for _, which := range []string{"tenant", "file", "message", "conversation", "membership"} {
		t.Run(which, func(t *testing.T) {
			c := fileDownloadDB(t)
			m, mid := downloadFixture(t, c)
			switch which {
			case "tenant":
				m.TenantID = tenantB
			case "file":
				m.ID = freshFile().ID
			case "message":
				mid = freshFile().ID
			case "conversation":
				m.ConversationID = directB
			case "membership":
				run(t, c, `UPDATE user_organizations SET user_id=$2 WHERE id=$1`, adminM, adminA)
			}
			if which == "membership" {
				_, e := c.Exec(context.Background(), `INSERT INTO file_download_sessions(id,tenant_id,file_id,conversation_id,message_id,requester_user_id,source_membership_id,owner_id,lease_token,phase,expected_bytes,created_at,updated_at,deadline,lease_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$1,$1,'preparing',1,$8,$8,$9,$9)`, freshFile().ID, tenantA, m.ID, directA, mid, personB, adminM, at, at.Add(time.Minute))
				if e == nil {
					t.Fatal("wrong requester membership accepted")
				}
				return
			}
			_, e := insertDownloadSession(c, m, mid, "preparing")
			if e == nil {
				t.Fatal("cross origin accepted", which)
			}
		})
	}
}
func TestFileDownloadSchemaStages(t *testing.T) {
	for _, phase := range []string{"authorized", "completed", "interrupted", "unknown"} {
		t.Run("initial_"+phase, func(t *testing.T) {
			c := fileDownloadDB(t)
			m, mid := downloadFixture(t, c)
			_, e := insertDownloadSession(c, m, mid, phase)
			if e == nil {
				t.Fatal("terminal/authorized insertion bypassed preflight")
			}
		})
	}
	c := fileDownloadDB(t)
	m, mid := downloadFixture(t, c)
	id, e := insertDownloadSession(c, m, mid, "preparing")
	if e != nil {
		t.Fatal(e)
	}
	for _, set := range []string{"phase='authorized'", "phase='completed',bytes_written=expected_bytes,reason_code='completed'", "deadline=deadline+interval '1 second'", "expected_bytes=NULL", "lease_expires_at='infinity'", "bytes_written=-1", "requester_user_id=NULL"} {
		if _, e = c.Exec(context.Background(), "UPDATE file_download_sessions SET "+set+" WHERE id=$1", id); e == nil {
			t.Fatal("illegal session mutation", set)
		}
	}
}
func TestFileDownloadSchemaTerminalImmutable(t *testing.T) {
	c := fileDownloadDB(t)
	m, mid := downloadFixture(t, c)
	id, e := insertDownloadSession(c, m, mid, "preparing")
	if e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(context.Background(), `UPDATE file_download_sessions SET phase='interrupted',reason_code='integrity_mismatch',updated_at=$2 WHERE id=$1`, id, at.Add(61*time.Second)); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(context.Background(), `INSERT INTO file_download_terminal_events(tenant_id,file_id,session_id,outcome,reason_code,bytes_written,occurred_at) VALUES($1,$2,$3,'interrupted','integrity_mismatch',0,$4)`, tenantA, m.ID, id, at.Add(61*time.Second)); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{`UPDATE file_download_sessions SET phase='completed',reason_code='completed',bytes_written=1 WHERE id=$1`, `DELETE FROM file_download_sessions WHERE id=$1`, `UPDATE file_download_terminal_events SET reason_code='timeout' WHERE session_id=$1`, `DELETE FROM file_download_terminal_events WHERE session_id=$1`, `UPDATE file_download_sessions SET audit_acked=true WHERE id=$1`} {
		if _, e = c.Exec(context.Background(), q, id); e == nil {
			t.Fatal("terminal fact/gate changed", q)
		}
	}
	if _, e = insertDownloadSession(c, m, mid, "preparing"); e == nil {
		t.Fatal("unacknowledged terminal gate opened")
	}
}
func TestFileRetentionSchemaHistory(t *testing.T) {
	c := fileDownloadDB(t)
	seed(t, c)
	var days, version int64
	var enabled bool
	if e := c.QueryRow(context.Background(), `SELECT file_retention_days,cleanup_enabled,version FROM tenant_file_retention_policy WHERE tenant_id=$1`, tenantA).Scan(&days, &enabled, &version); e != nil || days != 365 || enabled || version != 0 {
		t.Fatal(days, enabled, version, e)
	}
	for _, set := range []string{"file_retention_days=0", "file_retention_days=3651", "version=-1", "version=1,approval_reference=NULL", "cleanup_enabled=NULL"} {
		if _, e := c.Exec(context.Background(), "UPDATE tenant_file_retention_policy SET "+set+" WHERE tenant_id=$1", tenantA); e == nil {
			t.Fatal("policy NULL/bounds bypass", set)
		}
	}
	run(t, c, `INSERT INTO tenant_file_retention_policy_history(tenant_id,version,file_retention_days,cleanup_enabled,approval_reference,actor_user_id,acting_membership_id,updated_at) VALUES($1,1,365,false,'approved',$2,$3,$4)`, tenantA, adminA, adminM, at)
	reject(t, c, `UPDATE tenant_file_retention_policy_history SET file_retention_days=1`)
	reject(t, c, `DELETE FROM tenant_file_retention_policy_history`)
}
func TestFileDeleteSchemaCommitmentBarrier(t *testing.T) {
	for _, holdFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "hold_first", false: "commit_first"}[holdFirst], func(t *testing.T) {
			c := fileDownloadDB(t)
			seedDirectConversation(t, c)
			m := storedFile(t, c, "delete_pending")
			job := insertDeleteJob(t, c, m)
			run(t, c, `UPDATE tenant_file_retention_policy SET cleanup_enabled=true,version=1,approval_reference='approved',actor_user_id=$2,acting_membership_id=$3 WHERE tenant_id=$1`, tenantA, adminA, adminM)
			run(t, c, `UPDATE file_delete_jobs SET phase='inventory',inventory_exhausted=true,source_safe=true,policy_version=1 WHERE id=$1`, job)
			run(t, c, `INSERT INTO file_delete_versions(tenant_id,file_id,conversation_id,job_id,object_version_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, tenantA, m.ID, directA, job, m.ObjectVersionID, at)
			if holdFirst {
				if e := rawSchemaHold(c, directA); e != nil {
					t.Fatal(e)
				}
			}
			_, e := c.Exec(context.Background(), `UPDATE file_delete_versions SET phase='committed',commitment_id=$2,committed_at=$3,updated_at=$3 WHERE job_id=$1`, job, freshFile().ID, at.Add(time.Second))
			if holdFirst {
				if e == nil {
					t.Fatal("commit crossed active hold")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = rawSchemaHold(c, directA); e == nil {
				t.Fatal("direct SQL hold ignored delete commitment")
			}
			run(t, c, `UPDATE file_delete_jobs SET lease_expires_at=$2,updated_at=$3 WHERE id=$1`, job, at.Add(120*time.Second), at.Add(time.Second))
			run(t, c, `UPDATE tenant_file_retention_policy SET cleanup_enabled=false,version=2 WHERE tenant_id=$1`, tenantA)
			if e = rawSchemaHold(c, directA); e == nil {
				t.Fatal("lease/config pause removed barrier")
			}
			reject(t, c, `UPDATE file_delete_versions SET phase='inventoried',commitment_id=NULL,committed_at=NULL WHERE job_id=$1`, job)
		})
	}
}
func TestFileDownloadDownGuards(t *testing.T) {
	for _, which := range []string{"default", "session", "history", "job", "rule"} {
		t.Run(which, func(t *testing.T) {
			c := fileDownloadDB(t)
			if which == "default" {
				seed(t, c)
				if e := fileDownloadMigration(t, c, "down"); e != nil {
					t.Fatal(e)
				}
				if e := fileDownloadMigration(t, c, "up"); e != nil {
					t.Fatal(e)
				}
				return
			}
			if which == "rule" {
				seed(t, c)
				run(t, c, `INSERT INTO policy_versions(tenant_id,version,status,published_by_user_id,reason) VALUES($1,1,'draft',$2,'test')`, tenantA, adminA)
				run(t, c, `INSERT INTO policy_rules(tenant_id,version,rule_id,effect,action,reason,effective_from) VALUES($1,1,'download-block','hard_deny','file_download','test',$2)`, tenantA, at)
			} else if which == "history" {
				seed(t, c)
				run(t, c, `INSERT INTO tenant_file_retention_policy_history(tenant_id,version,file_retention_days,cleanup_enabled,approval_reference,actor_user_id,acting_membership_id,updated_at) VALUES($1,1,365,false,'approved',$2,$3,$4)`, tenantA, adminA, adminM, at)
			} else {
				m, mid := downloadFixture(t, c)
				if which == "session" {
					if _, e := insertDownloadSession(c, m, mid, "preparing"); e != nil {
						t.Fatal(e)
					}
				} else {
					m = fileNext(m, "delete_pending")
					if e := writeFile(c, m, false); e != nil {
						t.Fatal(e)
					}
					insertDeleteJob(t, c, m)
				}
			}
			expectFileSQLState(t, fileDownloadMigration(t, c, "down"), "23514")
			run(t, c, "ROLLBACK")
		})
	}
}
func TestFileDownloadDownConcurrentInsert(t *testing.T) {
	c := fileDownloadDB(t)
	m, mid := downloadFixture(t, c)
	tx, e := c.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	runTx := func(q string, args ...any) {
		if _, e := tx.Exec(context.Background(), q, args...); e != nil {
			t.Fatal(e)
		}
	}
	runTx(`LOCK TABLE file_download_sessions IN ROW EXCLUSIVE MODE`)
	second, e := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close(context.Background())
	var search string
	if e = c.QueryRow(context.Background(), "SHOW search_path").Scan(&search); e != nil {
		t.Fatal(e)
	}
	if _, e = second.Exec(context.Background(), "SET search_path TO "+search); e != nil {
		t.Fatal(e)
	}
	run(t, second, "SET lock_timeout='100ms'")
	expectFileSQLState(t, fileDownloadMigration(t, second, "down"), "55P03")
	run(t, second, "ROLLBACK")
	id := freshFile().ID
	runTx(`INSERT INTO file_download_sessions(id,tenant_id,file_id,conversation_id,message_id,requester_user_id,source_membership_id,owner_id,lease_token,phase,expected_bytes,created_at,updated_at,deadline,lease_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$1,$1,'preparing',1,$8,$8,$9,$9)`, id, tenantA, m.ID, directA, mid, adminA, adminM, at, at.Add(time.Minute))
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	expectFileSQLState(t, fileDownloadMigration(t, second, "down"), "23514")
	run(t, second, "ROLLBACK")
}

func TestFileDownloadSchemaAuthorizationAndAck(t *testing.T) {
	c := fileDownloadDB(t)
	m, mid := downloadFixture(t, c)
	id, e := insertDownloadSession(c, m, mid, "preparing")
	if e != nil {
		t.Fatal(e)
	}
	var auditID int64
	if e = c.QueryRow(context.Background(), `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,'file_download_authorize','file',$4,'allow','download_authorized',$5) RETURNING id`, tenantA, adminA, adminM, m.ID, at.Add(61*time.Second)).Scan(&auditID); e != nil {
		t.Fatal(e)
	}
	run(t, c, `UPDATE file_download_sessions SET phase='authorized',authorized_audit_id=$2,updated_at=$3 WHERE id=$1`, id, auditID, at.Add(61*time.Second))
	run(t, c, "BEGIN")
	run(t, c, `UPDATE file_download_sessions SET phase='completed',bytes_written=expected_bytes,reason_code='completed',updated_at=$2 WHERE id=$1`, id, at.Add(62*time.Second))
	run(t, c, `INSERT INTO file_download_terminal_events(tenant_id,file_id,session_id,outcome,reason_code,bytes_written,occurred_at) VALUES($1,$2,$3,'completed','completed',1,$4)`, tenantA, m.ID, id, at.Add(62*time.Second))
	run(t, c, "COMMIT")
	if _, e = insertDownloadSession(c, m, mid, "preparing"); e == nil {
		t.Fatal("terminal audit missing gate")
	}
	var machineAudit int64
	if e = c.QueryRow(context.Background(), `INSERT INTO file_worker_audit_events(tenant_id,file_id,worker_id,job_id,operation,reason_code,outcome,occurred_at,download_session_id) VALUES($1,$2,$3,$4,'download_terminal','completed','allow',$5,$4) RETURNING id`, tenantA, m.ID, freshFile().ID, id, at.Add(63*time.Second)).Scan(&machineAudit); e != nil {
		t.Fatal(e)
	}
	run(t, c, "BEGIN")
	run(t, c, `UPDATE file_download_terminal_events SET ack_audit_id=$2 WHERE session_id=$1`, id, machineAudit)
	run(t, c, `UPDATE file_download_sessions SET audit_acked=true WHERE id=$1`, id)
	run(t, c, "COMMIT")
	if _, e = insertDownloadSession(c, m, mid, "preparing"); e != nil {
		t.Fatal("acked terminal gate stayed closed", e)
	}
	reject(t, c, `UPDATE file_download_terminal_events SET ack_audit_id=NULL WHERE session_id=$1`, id)
	reject(t, c, `UPDATE audit_events SET reason='changed' WHERE id=$1`, auditID)
	reject(t, c, `DELETE FROM audit_events WHERE id=$1`, auditID)
}
func TestFileDeleteSchemaSingleUnresolvedVersion(t *testing.T) {
	c := fileDownloadDB(t)
	seedDirectConversation(t, c)
	run(t, c, `UPDATE tenant_file_retention_policy SET cleanup_enabled=true,version=1,approval_reference='approved',actor_user_id=$2,acting_membership_id=$3 WHERE tenant_id=$1`, tenantA, adminA, adminM)
	for i := 0; i < 2; i++ {
		m := storedFile(t, c, "delete_pending")
		job := insertDeleteJob(t, c, m)
		run(t, c, `UPDATE file_delete_jobs SET phase='inventory',inventory_exhausted=true,source_safe=true,policy_version=1 WHERE id=$1`, job)
		run(t, c, `INSERT INTO file_delete_versions(tenant_id,file_id,conversation_id,job_id,object_version_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, tenantA, m.ID, directA, job, m.ObjectVersionID, at)
		_, e := c.Exec(context.Background(), `UPDATE file_delete_versions SET phase='committed',commitment_id=$2,committed_at=$3,updated_at=$3 WHERE job_id=$1`, job, freshFile().ID, at.Add(time.Second))
		if i == 0 && e != nil {
			t.Fatal(e)
		}
		if i == 1 {
			expectFileSQLState(t, e, "23505")
		}
		reject(t, c, `INSERT INTO file_delete_versions(tenant_id,file_id,conversation_id,job_id,object_version_id,phase,commitment_id,committed_at,absence_checked_at,created_at,updated_at) VALUES($1,$2,$3,$4,'unproven','absent',$5,$6,$6,$6,$6)`, tenantA, m.ID, directA, job, freshFile().ID, at)
	}
}
