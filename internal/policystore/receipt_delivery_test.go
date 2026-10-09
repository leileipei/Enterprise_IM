package policystore_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestDeliveredRejectsMissingPageAndForgedProof(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	if _, e := s.SendTextMessage(ctx, publisher(), directA, clientUUIDv7(at, 2), "第二条"); e != nil {
		t.Fatal(e)
	}
	later, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 1, 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, later.SyncProof); !errors.Is(e, policystore.ErrSyncGap) {
		t.Fatal("missing first page", e)
	}
	first, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, strings.Repeat("A", 43)); !errors.Is(e, policystore.ErrSyncProofNotFound) {
		t.Fatal("forged proof", e)
	}
	g, e := s.RegisterDevice(ctx, groupMemberIdentity(), deviceRegistration(2))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}, directA, first.SyncProof); !errors.Is(e, policystore.ErrSyncProofNotFound) {
		t.Fatal("different device", e)
	}
	if _, e = s.ConfirmDelivered(ctx, deviceIdentity(), device, directA, first.SyncProof); !errors.Is(e, policystore.ErrSyncProofNotFound) {
		t.Fatal("different acting membership", e)
	}
	state, e := s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, first.SyncProof)
	if e != nil || state.ScannedSeq != 1 || state.DeliveredSeq != 1 || state.ReceiptRevision != 1 || state.Duplicate {
		t.Fatal("first delivery", state, e)
	}
	state, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, later.SyncProof)
	if e != nil || state.ScannedSeq != 2 || state.DeliveredSeq != 2 || state.ReceiptRevision != 2 {
		t.Fatal("second page", state, e)
	}
	state, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, first.SyncProof)
	if e != nil || state.ScannedSeq != 2 || !state.Duplicate || state.ReceiptRevision != 2 {
		t.Fatal("replay regressed", state, e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM receipt_outbox_targets").Scan(&n); e != nil || n != 4 {
		t.Fatal("targets must be sender and owner", n, e)
	}
}
func TestDeliveredConcurrentReplayProducesOneUserFact(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
	if e != nil {
		t.Fatal(e)
	}
	peers := make([]*pgx.Conn, 8)
	for i := range peers {
		peers[i] = filePeer(t, c)
	}
	var wg sync.WaitGroup
	done := make(chan error, len(peers))
	start := make(chan struct{})
	for _, peer := range peers {
		wg.Add(1)
		go func(p *pgx.Conn) {
			defer wg.Done()
			<-start
			_, e := deviceService(p).ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof)
			done <- e
		}(peer)
	}
	close(start)
	wg.Wait()
	close(done)
	for e := range done {
		if e != nil {
			t.Fatal(e)
		}
	}
	for _, table := range []string{"message_device_receipts", "message_user_receipts", "receipt_outbox_events"} {
		var n int
		if e = c.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 1 {
			t.Fatal("replay duplication", table, n, e)
		}
	}
}
func TestDeliveredProofExpiryAndRevocationAtCommit(t *testing.T) {
	for _, which := range []string{"expired", "revoked", "late_expiry"} {
		t.Run(which, func(t *testing.T) {
			c, s, device := receiptDirectFixture(t)
			ctx := context.Background()
			page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
			if e != nil {
				t.Fatal(e)
			}
			if which == "expired" {
				if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); e != nil {
					t.Fatal(e)
				}
				s.Now = func() time.Time { return page.ProofExpiresAt }
				if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); !errors.Is(e, policystore.ErrSyncProofExpired) {
					t.Fatal(e)
				}
				var n int
				if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts").Scan(&n); e != nil || n != 1 {
					t.Fatal("expired replay erased facts")
				}
			} else if which == "revoked" {
				if e = s.RevokeDevice(ctx, deviceOwner(), device.ID); e != nil {
					t.Fatal(e)
				}
				if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); !errors.Is(e, policystore.ErrInvalidDevice) {
					t.Fatal(e)
				}
			} else {
				clock := at
				s.Now = func() time.Time { return clock }
				s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = page.ProofExpiresAt }}
				if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); !errors.Is(e, policystore.ErrSyncProofExpired) {
					t.Fatal("expiry after audit", e)
				}
				var n int
				if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts").Scan(&n); e != nil || n != 0 {
					t.Fatal("late confirmation committed")
				}
			}
		})
	}
}
func TestDeliveredGroupInvisibleIntervalsAndMembershipSwitch(t *testing.T) {
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
	insertGroupHistoryMessage(t, c, group.ID, 2, adminA, adminM, "离群缺口")
	if _, e = s.InviteGroupMember(ctx, publisher(), group.ID, inviteGroupRequest(targetM2)); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 3, adminA, adminM, "重新加入")
	g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	device := policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}
	page, e := s.SyncDeviceMessages(ctx, deviceIdentity(), device, group.ID, 0, 100)
	if e != nil || len(page.Page.Messages) != 3 || !page.Page.Messages[1].Redacted {
		t.Fatal(e)
	}
	state, e := s.ConfirmDelivered(ctx, deviceIdentity(), device, group.ID, page.SyncProof)
	if e != nil || state.ScannedSeq != 3 || state.DeliveredSeq != 3 {
		t.Fatal(state, e)
	}
	var seqs []int64
	rows, e := c.Query(ctx, "SELECT seq FROM message_user_receipts ORDER BY seq")
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		var seq int64
		if e = rows.Scan(&seq); e != nil {
			t.Fatal(e)
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 3 {
		t.Fatal("gap receipt invented", seqs)
	}
}
func TestDeliveredContextChangedAndAuditRollback(t *testing.T) {
	for _, which := range []string{"cleared", "retention", "audit", "outbox"} {
		t.Run(which, func(t *testing.T) {
			c, s, device := receiptDirectFixture(t)
			ctx := context.Background()
			if which == "retention" {
				grantPublisher(t, c)
				if _, e := (access.Service{DB: c, Now: func() time.Time { return at }}).SetRetentionPolicy(ctx, publisher(), 0, 1, "test"); e != nil {
					t.Fatal(e)
				}
				s.Now = func() time.Time { return at.Add(24*time.Hour - time.Second) }
			}
			page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
			if e != nil {
				t.Fatal(e)
			}
			expected := policystore.ErrSyncContextChanged
			switch which {
			case "cleared":
				run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$1", at)
			case "retention":
				s.Now = func() time.Time { return at.Add(24 * time.Hour) }
			case "audit", "outbox":
				table := "audit_events"
				if which == "outbox" {
					table = "receipt_outbox_events"
				}
				run(t, c, `CREATE FUNCTION fail_receipt_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'write unavailable'; END $$`)
				run(t, c, "CREATE TRIGGER fail_receipt_write BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION fail_receipt_write()")
				expected = nil
			}
			if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); e == nil || (expected != nil && !errors.Is(e, expected)) {
				t.Fatal("partial acceptance", which, e)
			}
			for _, table := range []string{"message_device_receipts", "message_user_receipts", "receipt_outbox_events"} {
				var n int
				if e = c.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
					t.Fatal("rollback leaked", table, n, e)
				}
			}
		})
	}
}

func TestDeliveredFiveHundredAndInt64Sequences(t *testing.T) {
	t.Run("500 messages", func(t *testing.T) {
		c, s, device := receiptDirectFixture(t)
		ctx := context.Background()
		run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id)
 SELECT $1,$2,seq,$3,$4,('0199f04a-0000-7000-8000-'||lpad(to_hex(seq),12,'0'))::uuid,'批量',decode(repeat('ab',32),'hex'),$5,$6,$7,$8,$8 FROM generate_series(2,500) seq`, tenantA, directA, adminA, adminM, at, personA, targetM2, orgA)
		run(t, c, "UPDATE conversations SET last_seq=500 WHERE id=$1", directA)
		page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 500)
		if e != nil || len(page.Page.Messages) != 500 {
			t.Fatal("500 sync", e)
		}
		state, e := s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof)
		if e != nil || state.ScannedSeq != 500 || state.DeliveredSeq != 500 || state.ReceiptRevision != 1 {
			t.Fatal("500 delivery", state, e)
		}
		var n int
		if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts").Scan(&n); e != nil || n != 500 {
			t.Fatal("500 facts", n, e)
		}
	})
	t.Run("int64 sparse sequence", func(t *testing.T) {
		c, s, device := receiptDirectFixture(t)
		ctx := context.Background()
		first, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, first.SyncProof); e != nil {
			t.Fatal(e)
		}
		const seq int64 = 9007199254740993
		run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id) VALUES($1,$2,$3,$4,$5,$6,'大序号',decode(repeat('ab',32),'hex'),$7,$8,$9,$10,$10)`, tenantA, directA, seq, adminA, adminM, clientUUIDv7(at, 9001), at, personA, targetM2, orgA)
		run(t, c, "UPDATE conversations SET last_seq=$2 WHERE id=$1", directA, seq)
		page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 1, 100)
		if e != nil || page.Page.NextAfterSeq != seq {
			t.Fatal("big sequence", e)
		}
		state, e := s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof)
		if e != nil || state.ScannedSeq != seq || state.DeliveredSeq != seq {
			t.Fatal("big delivery", state, e)
		}
		var n int
		if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts").Scan(&n); e != nil || n != 2 {
			t.Fatal("sparse sequence fabricated facts")
		}
	})
}

func TestDeliveredFutureHardDenyAfterAuditRollsBack(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	grantPublisher(t, c)
	rule := policy.Rule{ID: "future-receipt-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "test"}
	if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{rule}, "test"); e != nil {
		t.Fatal(e)
	}
	page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
	if e != nil {
		t.Fatal(e)
	}
	clock := at
	s.Now = func() time.Time { return clock }
	s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = at.Add(time.Second) }}
	if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); !errors.Is(e, policystore.ErrSyncContextChanged) {
		t.Fatal("late hard deny", e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts").Scan(&n); e != nil || n != 0 {
		t.Fatal("hard deny committed partial facts")
	}
}

type afterReceiptStateDB struct {
	access.Beginner
	after func()
}

func (d afterReceiptStateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return afterReceiptStateTx{Tx: tx, after: d.after}, nil
}

type afterReceiptStateTx struct {
	pgx.Tx
	after func()
}

func (tx afterReceiptStateTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, q, args...)
	if strings.Contains(q, "COALESCE((SELECT scanned_seq") {
		return afterReceiptStateRow{Row: row, after: tx.after}
	}
	return row
}

type afterReceiptStateRow struct {
	pgx.Row
	after func()
}

func (r afterReceiptStateRow) Scan(args ...any) error {
	e := r.Row.Scan(args...)
	if e == nil {
		r.after()
	}
	return e
}
func TestDeliveredFinalClockAfterStateQuery(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "replay"}[replay], func(t *testing.T) {
			c, s, device := receiptDirectFixture(t)
			ctx := context.Background()
			page, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), device, directA, 0, 1)
			if e != nil {
				t.Fatal(e)
			}
			if replay {
				if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); e != nil {
					t.Fatal(e)
				}
			}
			clock := at
			s.Now = func() time.Time { return clock }
			s.DB = afterReceiptStateDB{Beginner: c, after: func() { clock = page.ProofExpiresAt }}
			if _, e = s.ConfirmDelivered(ctx, groupMemberIdentity(), device, directA, page.SyncProof); !errors.Is(e, policystore.ErrSyncProofExpired) {
				t.Fatal("commit-time expiry ignored", e)
			}
			var n int
			want := 0
			if replay {
				want = 1
			}
			if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts").Scan(&n); e != nil || n != want {
				t.Fatal("final expiry leaked or erased facts", n, e)
			}
		})
	}
}
