package policystore_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func receiptDirectFixture(t *testing.T) (*pgx.Conn, policystore.Service, policystore.DeviceCredential) {
	t.Helper()
	c := db(t)
	seedDirectConversation(t, c)
	s := deviceService(c)
	g, e := s.RegisterDevice(context.Background(), groupMemberIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 1), "第一条"); e != nil {
		t.Fatal(e)
	}
	return c, s, policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}
}
func TestSyncProofMatchesFinalVisiblePage(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	if _, e := s.SendTextMessage(ctx, groupMemberIdentity(), directA, clientUUIDv7(at, 2), "自己发送"); e != nil {
		t.Fatal(e)
	}
	run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at) VALUES($1,$2,3,$3,$4,$5,'legacy',decode(repeat('ab',32),'hex'),$6)`, tenantA, directA, adminA, adminM, clientUUIDv7(at, 3), at)
	run(t, c, "UPDATE conversations SET last_seq=3 WHERE id=$1", directA)
	page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 100)
	if e != nil || page.SyncProof == "" || len(page.Page.Messages) != 3 || page.Page.NextAfterSeq != 3 || !page.Page.Messages[2].Redacted || !page.ProofExpiresAt.Equal(at.Add(300*time.Second)) {
		t.Fatal("sync page", e)
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(page.SyncProof)
	if e != nil || len(raw) != 32 {
		t.Fatal("proof encoding")
	}
	digest := sha256.Sum256(raw)
	var owner, proofDevice, member string
	var count int
	var seq int64
	if e = c.QueryRow(ctx, "SELECT user_id::text,device_id::text,acting_membership_id::text FROM device_sync_proofs WHERE token_digest=$1", digest[:]).Scan(&owner, &proofDevice, &member); e != nil || owner != personA || proofDevice != device.ID || member != targetM2 {
		t.Fatal("proof scope", e)
	}
	if e = c.QueryRow(ctx, "SELECT count(*),max(seq) FROM device_sync_proof_items").Scan(&count, &seq); e != nil || count != 1 || seq != 1 {
		t.Fatal("proof items include redacted/self messages", e)
	}
	lease := page.ContentLeases[1]
	if !lease.CacheUntil.Equal(at.Add(300*time.Second)) || !lease.ContentExpiresAt.Equal(at.Add(365*24*time.Hour)) {
		t.Fatal("content lease")
	}
	empty, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 3, 100)
	if e != nil || empty.SyncProof != "" || len(empty.Page.Messages) != 0 {
		t.Fatal("empty proof", e)
	}
}
func TestSyncProofQuotaAndAuditRollback(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if _, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1); !errors.Is(e, policystore.ErrSyncProofQuota) {
		t.Fatal("proof quota", e)
	}
	s.Now = func() time.Time { return at.Add(300 * time.Second) }
	if _, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1); e != nil {
		t.Fatal("expiry frees proof quota", e)
	}
	run(t, c, `CREATE FUNCTION fail_sync_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER fail_sync_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_sync_audit()`)
	if _, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	var n int
	if e := c.QueryRow(ctx, "SELECT count(*) FROM device_sync_proofs").Scan(&n); e != nil || n != 21 {
		t.Fatal("unaudited proof committed")
	}
}
func TestSyncProofFinalVisibilityAfterAudit(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	grantPublisher(t, c)
	if _, e := (access.Service{DB: c, Now: func() time.Time { return at }}).SetRetentionPolicy(ctx, publisher(), 0, 1, "test"); e != nil {
		t.Fatal(e)
	}
	clock := at.Add(24*time.Hour - time.Second)
	s.Now = func() time.Time { return clock }
	s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = at.Add(24 * time.Hour) }}
	page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
	if e != nil || len(page.Page.Messages) != 1 || !page.Page.Messages[0].Redacted || len(page.ContentLeases) != 0 {
		t.Fatal("audit-time expiry leaked content", e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM device_sync_proof_items").Scan(&n); e != nil || n != 0 {
		t.Fatal("proof did not match final page")
	}
}

func TestSyncProofConcurrentQuotaIsAtomic(t *testing.T) {
	c, _, device := receiptDirectFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	peers := make([]*pgx.Conn, 24)
	for i := range peers {
		peers[i] = filePeer(t, c)
	}
	done := make(chan error, 24)
	start := make(chan struct{})
	for _, p := range peers {
		go func(p *pgx.Conn) {
			<-start
			_, e := deviceService(p).SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
			done <- e
		}(p)
	}
	close(start)
	success := 0
	for range peers {
		e := <-done
		if e == nil {
			success++
		} else if !errors.Is(e, policystore.ErrSyncProofQuota) {
			t.Fatal(e)
		}
	}
	if success != 20 {
		t.Fatal("proof quota raced", success)
	}
	var tokenDigest []byte
	if e := c.QueryRow(ctx, "SELECT token_digest FROM device_sync_proofs LIMIT 1").Scan(&tokenDigest); e != nil || len(tokenDigest) != 32 {
		t.Fatal("proof digest")
	}
}
