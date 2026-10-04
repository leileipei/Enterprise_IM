package retention

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These are database-boundary fixtures, not scanner acceptance evidence.
func seedFileDigestCandidate(t *testing.T, pool *pgxpool.Pool, seq int64, expiry time.Time, clear bool) string {
	t.Helper()
	ctx := context.Background()
	fid := fmt.Sprintf("00000000-0000-4000-8000-%012x", 90000+seq)
	created := fixedTime.Add(-400 * 24 * time.Hour)
	exec(t, pool, `INSERT INTO file_objects(id,tenant_id,conversation_id,uploader_user_id,uploader_membership_id,upload_request_id,request_digest,original_filename,declared_media_type,declared_size_bytes,state,state_version,created_at,updated_at,upload_expires_at)
 VALUES($1,$2,$3,$4,$5,$1,decode(repeat('ab',32),'hex'),'报告.pdf','application/pdf',1,'allocated',0,$6,$6,$7)`, fid, tenantA, conversationA, userA, memberA, created, created.Add(time.Hour))
	exec(t, pool, `UPDATE file_objects SET state='uploaded',state_version=1,updated_at=$2,uploaded_at=$2,actual_size_bytes=1,object_key='tenants/'||tenant_id::text||'/files/'||id::text,object_version_id='fixture-v1',detected_media_type='application/pdf',sha256=decode(repeat('ab',32),'hex') WHERE id=$1`, fid, created.Add(time.Second))
	exec(t, pool, `UPDATE file_objects SET state='scanning',state_version=2,updated_at=$2,scan_job_id=$1 WHERE id=$1`, fid, created.Add(2*time.Second))
	exec(t, pool, `UPDATE file_objects SET state='ready',state_version=3,updated_at=$2,scanned_at=$2,scan_engine='fixture-engine',scan_definition_version='fixture-definitions',scan_sha256=sha256 WHERE id=$1`, fid, created.Add(3*time.Second))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var mid string
	client := fmt.Sprintf("0199f04a-0000-7000-8000-%012x", seq)
	err = tx.QueryRow(ctx, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,message_type,text_body,content_digest,accepted_at)
 VALUES($1,$2,$3,$4,$5,$6,'file','',decode(repeat('ab',32),'hex'),$7) RETURNING id::text`, tenantA, conversationA, seq, userA, memberA, client, fixedTime.Add(-366*24*time.Hour)).Scan(&mid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO message_idempotency(tenant_id,conversation_id,sender_user_id,client_msg_id,message_id,content_digest,accepted_at,expires_at) SELECT tenant_id,conversation_id,sender_user_id,client_msg_id,id,content_digest,accepted_at,$2 FROM messages WHERE id=$1`, mid, expiry)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO message_attachments(tenant_id,conversation_id,message_id,sender_user_id,sender_membership_id,file_id,sealed_sha256) VALUES($1,$2,$3,$4,$5,$6,decode(repeat('ab',32),'hex'))`, tenantA, conversationA, mid, userA, memberA, fid)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, "UPDATE conversations SET last_seq=$2 WHERE id=$1", conversationA, seq)
	if clear {
		exec(t, pool, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", mid, fixedTime.Add(-time.Hour))
	}
	return mid
}
func assertFileDigestProof(t *testing.T, pool *pgxpool.Pool, mid string, retired bool) {
	t.Helper()
	var m, i, a []byte
	var ms, is, as *time.Time
	err := pool.QueryRow(context.Background(), `SELECT m.content_digest,i.content_digest,a.sealed_sha256,m.digest_retired_at,i.digest_retired_at,a.fingerprint_retired_at FROM messages m JOIN message_idempotency i ON i.tenant_id=m.tenant_id AND i.message_id=m.id JOIN message_attachments a ON a.tenant_id=m.tenant_id AND a.message_id=m.id WHERE m.id=$1`, mid).Scan(&m, &i, &a, &ms, &is, &as)
	if err != nil {
		t.Fatal(err)
	}
	if retired {
		if m != nil || i != nil || a != nil || ms == nil || is == nil || as == nil || !ms.Equal(*is) || !ms.Equal(*as) || !ms.Equal(fixedTime) {
			t.Fatal("partial retirement", ms, is, as)
		}
	} else if len(m) != 32 || len(i) != 32 || len(a) != 32 || ms != nil || is != nil || as != nil {
		t.Fatal("unexpected retirement", ms, is, as)
	}
}
func TestFileMessageDigestRetirementAtomic(t *testing.T) {
	pool := database(t)
	mid := seedFileDigestCandidate(t, pool, 1, fixedTime.Add(-time.Hour), false)
	body, err := testWorker(pool, 1).ProcessTenant(context.Background(), tenantA)
	if err != nil || body.ClearedCount != 1 {
		t.Fatal(body, err)
	}
	var caption *string
	if err = pool.QueryRow(context.Background(), "SELECT text_body FROM messages WHERE id=$1", mid).Scan(&caption); err != nil || caption != nil {
		t.Fatal("empty caption not cleared", err)
	}
	seedDigestCandidate(t, pool, conversationA, 2, fixedTime.Add(-time.Hour), true)
	b, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA)
	if err != nil || b.RetiredCount != 2 {
		t.Fatal(b, err)
	}
	assertFileDigestProof(t, pool, mid, true)
	assertDigestCounts(t, pool, 2, 1)
	var sha []byte
	if err = pool.QueryRow(context.Background(), "SELECT sha256 FROM file_objects").Scan(&sha); err != nil || len(sha) != 32 {
		t.Fatal("scan evidence incorrectly retired", err)
	}
}
func TestFileMessageDigestRetirementHold(t *testing.T) {
	for _, condition := range []string{"body present", "not expired", "active hold", "released hold"} {
		t.Run(condition, func(t *testing.T) {
			pool := database(t)
			expiry := fixedTime.Add(-time.Hour)
			if condition == "not expired" {
				expiry = fixedTime.Add(time.Hour)
			}
			mid := seedFileDigestCandidate(t, pool, 1, expiry, condition != "body present")
			if condition == "active hold" || condition == "released hold" {
				h := placeHold(t, pool, conversationA, 51)
				if condition == "released hold" {
					releaseHold(t, pool, conversationA, h, 51)
				}
			}
			b, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA)
			want := 0
			if condition == "released hold" {
				want = 1
			}
			if err != nil || b.RetiredCount != want {
				t.Fatal(b, err)
			}
			assertFileDigestProof(t, pool, mid, want == 1)
		})
	}
}
func TestFileMessageDigestRetirementRollback(t *testing.T) {
	for _, table := range []string{"message_attachments", "message_digest_retirement_batches"} {
		t.Run(table, func(t *testing.T) {
			pool := database(t)
			mid := seedFileDigestCandidate(t, pool, 1, fixedTime.Add(-time.Hour), true)
			exec(t, pool, `CREATE FUNCTION reject_retirement_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected retirement failure'; END $$`)
			op := "UPDATE"
			if table == "message_digest_retirement_batches" {
				op = "INSERT"
			}
			exec(t, pool, "CREATE TRIGGER fail_retirement BEFORE "+op+" ON "+table+" FOR EACH ROW EXECUTE FUNCTION reject_retirement_fixture()")
			if _, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA); err == nil {
				t.Fatal("fault committed")
			}
			assertFileDigestProof(t, pool, mid, false)
			assertDigestCounts(t, pool, 0, 0)
			exec(t, pool, "DROP TRIGGER fail_retirement ON "+table)
			if _, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA); err != nil {
				t.Fatal(err)
			}
			assertFileDigestProof(t, pool, mid, true)
		})
	}
	t.Run("missing returned attachment", func(t *testing.T) {
		pool := database(t)
		mid := seedFileDigestCandidate(t, pool, 1, fixedTime.Add(-time.Hour), true)
		exec(t, pool, `CREATE FUNCTION omit_retirement_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
		exec(t, pool, `CREATE TRIGGER omit_retirement BEFORE UPDATE ON message_attachments FOR EACH ROW EXECUTE FUNCTION omit_retirement_fixture()`)
		if _, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA); err == nil {
			t.Fatal("partial set committed")
		}
		assertFileDigestProof(t, pool, mid, false)
	})
}
func TestFileMessageDigestRetirementIrreversible(t *testing.T) {
	pool := database(t)
	mid := seedFileDigestCandidate(t, pool, 1, fixedTime.Add(-time.Hour), true)
	if _, err := testDigestWorker(pool, 0).ProcessTenant(context.Background(), tenantA); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"UPDATE message_attachments SET sealed_sha256=decode(repeat('ab',32),'hex'),fingerprint_retired_at=NULL WHERE message_id=$1", "DELETE FROM message_attachments WHERE message_id=$1", "UPDATE message_attachments SET fingerprint_retired_at=fingerprint_retired_at+interval '1 second' WHERE message_id=$1", "UPDATE messages SET content_digest=decode(repeat('ab',32),'hex'),digest_retired_at=NULL WHERE id=$1", "DELETE FROM messages WHERE id=$1"} {
		if _, err := pool.Exec(context.Background(), sql, mid); err == nil {
			t.Fatal("retired proof rewritten", sql)
		}
	}
	assertFileDigestProof(t, pool, mid, true)
}
