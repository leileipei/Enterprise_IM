package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func receiptDeliver(t *testing.T, s policystore.Service, device policystore.DeviceCredential, cid string) {
	t.Helper()
	ctx := context.Background()
	p, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, cid, 0, 500)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, cid, p.SyncProof); e != nil {
		t.Fatal(e)
	}
}
func TestReadRequiresOwnDeviceDeliveredAndCurrentVisibility(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("read before delivery", e)
	}
	if _, e := s.SendTextMessage(ctx, groupMemberIdentity(), directA, clientUUIDv7(at, 2), "自己发送"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SendTextMessage(ctx, publisher(), directA, clientUUIDv7(at, 3), "第三条"); e != nil {
		t.Fatal(e)
	}
	receiptDeliver(t, s, device, directA)
	g, e := s.RegisterDevice(ctx, groupMemberIdentity(), deviceRegistration(2))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}, directA, []int64{1}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("other device's delivery used", e)
	}
	for _, seqs := range [][]int64{{1, 2}, {1, 99}} {
		if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, seqs); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
			t.Fatal("whole batch must be denied", e)
		}
	}
	state, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{3})
	if e != nil || state.ReadSeq != 3 || state.ReceiptRevision != 2 || state.Duplicate {
		t.Fatal(state, e)
	}
	var read *time.Time
	if e = c.QueryRow(ctx, "SELECT read_at FROM message_user_receipts WHERE seq=1").Scan(&read); e != nil || read != nil {
		t.Fatal("high read cursor filled gap", e)
	}
	state, e = s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{3})
	if e != nil || !state.Duplicate || state.ReceiptRevision != 2 {
		t.Fatal("read replay", state, e)
	}
	// Expired sync proofs are unnecessary for reading a delivered message.
	s.Now = func() time.Time { return at.Add(301 * time.Second) }
	state, e = s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1})
	if e != nil || state.ReadSeq != 3 || state.ReceiptRevision != 3 {
		t.Fatal("lower read after proof expiry", state, e)
	}
	for _, seqs := range [][]int64{nil, {0}, {-1}, {1, 1}, {3, 1}, make([]int64, 501)} {
		if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, seqs); !errors.Is(e, policystore.ErrInvalidReceiptRequest) {
			t.Fatal("invalid seqs", e)
		}
	}
}
func TestReadAuditAndOutboxFailureRollBack(t *testing.T) {
	for _, table := range []string{"audit_events", "receipt_outbox_events"} {
		t.Run(table, func(t *testing.T) {
			c, s, device := receiptDirectFixture(t)
			ctx := context.Background()
			receiptDeliver(t, s, device, directA)
			run(t, c, `CREATE FUNCTION fail_read_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'write unavailable'; END $$`)
			run(t, c, "CREATE TRIGGER fail_read_write BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION fail_read_write()")
			if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1}); e == nil {
				t.Fatal("partial success")
			}
			for _, facts := range []string{"message_device_receipts", "message_user_receipts"} {
				var n int
				if e := c.QueryRow(ctx, "SELECT count(*) FROM "+facts+" WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 0 {
					t.Fatal("partial read", facts, e)
				}
			}
			var version int64
			if e := c.QueryRow(ctx, "SELECT revision FROM conversation_receipt_versions").Scan(&version); e != nil || version != 1 {
				t.Fatal("partial event", version, e)
			}
		})
	}
}
func TestReadBodyClearAndRevocationPreserveHistoricalFacts(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, device, directA)
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1}); e != nil {
		t.Fatal(e)
	}
	if e := s.RevokeDevice(ctx, deviceOwner(), device.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1}); !errors.Is(e, policystore.ErrInvalidDevice) {
		t.Fatal(e)
	}
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$1", at)
	g, e := s.RegisterDevice(ctx, groupMemberIdentity(), deviceRegistration(2))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}, directA, []int64{1}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("cleared body used", e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 1 {
		t.Fatal("history regressed")
	}
}
func TestReadGroupRejoinDoesNotCoverGap(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	interval := groupIntervalFor(t, c, group.ID, personA)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "第一条")
	if _, e = s.LeaveGroup(ctx, groupMemberIdentity(), group.ID, interval); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 2, adminA, adminM, "缺口")
	if _, e = s.InviteGroupMember(ctx, publisher(), group.ID, inviteGroupRequest(targetM2)); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 3, adminA, adminM, "再次加入")
	g, e := s.RegisterDevice(ctx, groupMemberIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	device := policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}
	receiptDeliver(t, s, device, group.ID)
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), device, group.ID, []int64{3}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), device, group.ID, []int64{1, 2}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("gap covered", e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 1 {
		t.Fatal("gap batch partially read", n, e)
	}
	if _, e = s.ConfirmRead(ctx, deviceIdentity(), device, group.ID, []int64{1}); e != nil {
		t.Fatal("legitimate other acting membership", e)
	}
}
func TestReadFinalDeviceExpiryAfterStateQuery(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, device, directA)
	clock := at
	s.Now = func() time.Time { return clock }
	s.DB = afterReceiptStateDB{Beginner: c, after: func() { clock = at.Add(30 * 24 * time.Hour) }}
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1}); !errors.Is(e, policystore.ErrInvalidDevice) {
		t.Fatal("late lease", e)
	}
	var n int
	if e := c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 0 {
		t.Fatal("expired device read committed")
	}
}

func TestReadFutureHardDenyAfterAuditRollsBack(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	receiptDeliver(t, s, device, directA)
	grantPublisher(t, c)
	rule := policy.Rule{ID: "future-read-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "test"}
	if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{rule}, "test"); e != nil {
		t.Fatal(e)
	}
	clock := at
	s.Now = func() time.Time { return clock }
	s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = at.Add(time.Second) }}
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("late hard deny", e)
	}
	var n int
	if e := c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 0 {
		t.Fatal("late deny committed read", n, e)
	}
}
func TestReadFiveHundredAndInt64Sequences(t *testing.T) {
	for _, bulk := range []bool{true, false} {
		name := "sparse int64"
		if bulk {
			name = "500 messages"
		}
		t.Run(name, func(t *testing.T) {
			c, s, device := receiptDirectFixture(t)
			ctx := context.Background()
			seqs := []int64{1, 9007199254740993}
			if bulk {
				seqs = make([]int64, 500)
				for i := range seqs {
					seqs[i] = int64(i + 1)
				}
				run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id)
 SELECT $1,$2,seq,$3,$4,('0199f04a-0000-7000-8000-'||lpad(to_hex(seq),12,'0'))::uuid,'批量',decode(repeat('ab',32),'hex'),$5,$6,$7,$8,$8 FROM generate_series(2,500) seq`, tenantA, directA, adminA, adminM, at, personA, targetM2, orgA)
			} else {
				run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id) VALUES($1,$2,$3,$4,$5,$6,'大序号',decode(repeat('ab',32),'hex'),$7,$8,$9,$10,$10)`, tenantA, directA, seqs[1], adminA, adminM, clientUUIDv7(at, 9001), at, personA, targetM2, orgA)
			}
			run(t, c, "UPDATE conversations SET last_seq=$2 WHERE id=$1", directA, seqs[len(seqs)-1])
			receiptDeliver(t, s, device, directA)
			state, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, seqs)
			if e != nil || state.ReadSeq != seqs[len(seqs)-1] || state.ReceiptRevision != 2 {
				t.Fatal(state, e)
			}
			var n int
			if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != len(seqs) {
				t.Fatal("invented/missing read facts", n, e)
			}
		})
	}
}
