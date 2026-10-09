package policystore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const receiptProofID = "00000000-0000-4000-8000-000000000291"

func seedReceiptSchema(t *testing.T, c *pgx.Conn) string {
	seedDirectConversation(t, c)
	run(t, c, `INSERT INTO messages(id,tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id)
 VALUES($1,$2,$3,1,$4,$5,$6,'消息',decode(repeat('ab',32),'hex'),$7,$8,$9,$10,$10)`, messageA, tenantA, directA, adminA, adminM, clientA, at, personA, targetM2, orgA)
	g, e := deviceService(c).RegisterDevice(context.Background(), deviceIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	return g.DeviceID
}
func insertReceiptProof(t *testing.T, c *pgx.Conn, device string) {
	run(t, c, `INSERT INTO device_sync_proofs(id,tenant_id,user_id,device_id,acting_membership_id,conversation_id,token_digest,after_seq,next_after_seq,issued_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,decode(repeat('cd',32),'hex'),0,1,$7,$8)`, receiptProofID, tenantA, personA, device, targetM2, directA, at, at.Add(300*time.Second))
}
func TestReceiptMigrationKeysAndMonotoneFacts(t *testing.T) {
	c := db(t)
	device := seedReceiptSchema(t, c)
	insertReceiptProof(t, c, device)
	proof := `INSERT INTO device_sync_proofs(id,tenant_id,user_id,device_id,acting_membership_id,conversation_id,token_digest,after_seq,next_after_seq,issued_at,expires_at) VALUES('00000000-0000-4000-8000-000000000292',$1,$2,$3,$4,$5,decode(repeat('ef',32),'hex'),0,1,$6,$7)`
	reject(t, c, proof, tenantA, adminA, device, adminM, directA, at, at.Add(300*time.Second))
	reject(t, c, proof, tenantB, personB, device, otherM, directA, at, at.Add(300*time.Second))
	reject(t, c, proof, tenantA, personA, device, adminM, directA, at, at.Add(300*time.Second))
	reject(t, c, proof, tenantA, personA, device, targetM2, directA, at, at.Add(301*time.Second))
	item := `INSERT INTO device_sync_proof_items(tenant_id,proof_id,conversation_id,user_id,device_id,acting_membership_id,seq,message_id,recipient_interval_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	run(t, c, item, tenantA, receiptProofID, directA, personA, device, targetM2, int64(1), messageA, nil)
	reject(t, c, item, tenantB, receiptProofID, directA, personA, device, targetM2, int64(1), messageA, nil)
	reject(t, c, item, tenantA, receiptProofID, directA, adminA, device, adminM, int64(1), messageA, nil)
	reject(t, c, item, tenantA, receiptProofID, directA, personA, device, targetM, int64(1), messageA, nil)
	reject(t, c, item, tenantA, receiptProofID, directA, personA, device, targetM2, int64(2), messageA, nil)
	reject(t, c, "UPDATE device_sync_proofs SET next_after_seq=2 WHERE id=$1", receiptProofID)
	reject(t, c, "UPDATE device_sync_proof_items SET seq=2 WHERE proof_id=$1", receiptProofID)
	cursor := `INSERT INTO device_delivery_cursors(tenant_id,user_id,device_id,conversation_id,acting_membership_id,scanned_seq,delivered_seq) VALUES($1,$2,$3,$4,$5,$6,$7)`
	run(t, c, cursor, tenantA, personA, device, directA, targetM2, int64(2), int64(1))
	reject(t, c, cursor, tenantA, adminA, device, directA, adminM, int64(2), int64(1))
	reject(t, c, "UPDATE device_delivery_cursors SET scanned_seq=1")
	reject(t, c, "UPDATE device_delivery_cursors SET delivered_seq=0")
	reject(t, c, "UPDATE device_delivery_cursors SET delivered_seq=3")
	run(t, c, "INSERT INTO conversation_user_read_cursors(tenant_id,user_id,conversation_id,read_seq) VALUES($1,$2,$3,1)", tenantA, personA, directA)
	reject(t, c, "UPDATE conversation_user_read_cursors SET read_seq=0")
	receipt := `INSERT INTO message_device_receipts(tenant_id,conversation_id,seq,user_id,device_id,delivered_at,read_at) VALUES($1,$2,$3,$4,$5,$6,$7)`
	run(t, c, receipt, tenantA, directA, int64(1), personA, device, at, nil)
	reject(t, c, receipt, tenantA, directA, int64(1), personA, device, at, nil)
	reject(t, c, receipt, tenantA, directA, int64(1), adminA, device, at, nil)
	reject(t, c, "UPDATE message_device_receipts SET read_at=delivered_at-interval '1 second'")
	run(t, c, "UPDATE message_device_receipts SET read_at=delivered_at+interval '1 second'")
	for _, sql := range []string{"UPDATE message_device_receipts SET read_at=NULL", "UPDATE message_device_receipts SET delivered_at=delivered_at+interval '1 second'", "UPDATE message_device_receipts SET read_at=read_at+interval '1 second'", "DELETE FROM message_device_receipts"} {
		reject(t, c, sql)
	}
	run(t, c, "INSERT INTO message_user_receipts(tenant_id,conversation_id,seq,user_id,delivered_at) VALUES($1,$2,1,$3,$4)", tenantA, directA, personA, at)
	reject(t, c, "INSERT INTO message_user_receipts(tenant_id,conversation_id,seq,user_id,delivered_at) VALUES($1,$2,1,$3,$4)", tenantA, directA, personA, at)
	run(t, c, "UPDATE message_user_receipts SET read_at=delivered_at")
	reject(t, c, "UPDATE message_user_receipts SET read_at=NULL")
	reject(t, c, "DELETE FROM message_user_receipts")
	run(t, c, "INSERT INTO conversation_receipt_versions(tenant_id,conversation_id,revision) VALUES($1,$2,1)", tenantA, directA)
	reject(t, c, "UPDATE conversation_receipt_versions SET revision=0")
}

func TestReceiptMigrationGroupProofScopeAndPageBinding(t *testing.T) {
	c := db(t)
	device := seedReceiptSchema(t, c)
	insertGroup(t, c)
	for _, v := range []struct{ id, user, member, role string }{{"00000000-0000-4000-8000-000000000295", personA, targetM2, "member"}, {"00000000-0000-4000-8000-000000000296", adminA, adminM, "owner"}} {
		if e := insertInterval(c, v.id, tenantA, groupA, v.user, v.member, orgA, legalA, v.role, "active", 1, nil); e != nil {
			t.Fatal(e)
		}
	}
	run(t, c, `INSERT INTO messages(id,tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at) VALUES($1,$2,$3,1,$4,$5,$6,'群消息',decode(repeat('ab',32),'hex'),$7)`, messageB, tenantA, groupA, adminA, adminM, clientB, at)
	run(t, c, `INSERT INTO device_sync_proofs(id,tenant_id,user_id,device_id,acting_membership_id,conversation_id,token_digest,after_seq,next_after_seq,issued_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,decode(repeat('cd',32),'hex'),0,1,$7,$8)`, receiptProofID, tenantA, personA, device, targetM2, groupA, at, at.Add(300*time.Second))
	item := `INSERT INTO device_sync_proof_items(tenant_id,proof_id,conversation_id,user_id,device_id,acting_membership_id,seq,message_id,recipient_interval_id) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8)`
	reject(t, c, item, tenantA, receiptProofID, directA, personA, device, targetM2, messageA, nil)
	reject(t, c, item, tenantA, receiptProofID, groupA, personA, device, targetM2, messageB, "00000000-0000-4000-8000-000000000296")
	reject(t, c, item, tenantA, receiptProofID, groupA, personA, device, targetM2, messageB, nil)
	run(t, c, item, tenantA, receiptProofID, groupA, personA, device, targetM2, messageB, "00000000-0000-4000-8000-000000000295")
}

func receiptMigration(t *testing.T, c *pgx.Conn, direction string) {
	t.Helper()
	b, e := os.ReadFile("../../db/migrations/000024_message_receipts." + direction + ".sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll(); e != nil {
		t.Fatal(e)
	}
}
func TestReceiptMigrationRollbackReapplyAndBodyClear(t *testing.T) {
	c := db(t)
	receiptMigration(t, c, "down")
	receiptMigration(t, c, "up")
	device := seedReceiptSchema(t, c)
	insertReceiptProof(t, c, device)
	run(t, c, `INSERT INTO device_sync_proof_items(tenant_id,proof_id,conversation_id,user_id,device_id,acting_membership_id,seq,message_id) VALUES($1,$2,$3,$4,$5,$6,1,$7)`, tenantA, receiptProofID, directA, personA, device, targetM2, messageA)
	run(t, c, "INSERT INTO message_device_receipts(tenant_id,conversation_id,seq,user_id,device_id,delivered_at,read_at) VALUES($1,$2,1,$3,$4,$5,$5)", tenantA, directA, personA, device, at)
	run(t, c, "INSERT INTO message_user_receipts(tenant_id,conversation_id,seq,user_id,delivered_at,read_at) VALUES($1,$2,1,$3,$4,$4)", tenantA, directA, personA, at)
	run(t, c, "DELETE FROM device_sync_proofs WHERE id=$1", receiptProofID)
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", messageA, at.AddDate(0, 0, 366))
	for _, table := range []string{"message_device_receipts", "message_user_receipts"} {
		var n int
		if e := c.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 1 {
			t.Fatal("facts lost", table, n, e)
		}
	}
	var n int
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM device_sync_proof_items").Scan(&n); e != nil || n != 0 {
		t.Fatal("proof items not removed")
	}
	reject(t, c, "UPDATE messages SET text_body='restored',body_cleared_at=NULL WHERE id=$1", messageA)
	down, e := os.ReadFile("../../db/migrations/000024_message_receipts.down.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.PgConn().Exec(context.Background(), string(down)).ReadAll(); e == nil {
		t.Fatal("rollback discarded business facts")
	}
	run(t, c, "ROLLBACK")
}
