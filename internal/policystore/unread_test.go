package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
	"time"
)

func TestUnreadUsesVisibleFactsAndBoundedLowerCount(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx := context.Background()
	if _, e := s.SendTextMessage(ctx, groupMemberIdentity(), directA, clientUUIDv7(at, 2), "自己"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SendTextMessage(ctx, publisher(), directA, clientUUIDv7(at, 3), "第三条"); e != nil {
		t.Fatal(e)
	}
	receiptDeliver(t, s, device, directA)
	if _, e := s.ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{3}); e != nil {
		t.Fatal(e)
	}
	got, e := s.GetUnread(ctx, groupMemberIdentity(), device, directA)
	if e != nil || !got.Exact || got.UnreadCount != 1 || got.HeadSeq != 3 || got.ReceiptRevision != 2 {
		t.Fatal(got, e)
	}
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$1 WHERE seq=1", at)
	got, e = s.GetUnread(ctx, groupMemberIdentity(), device, directA)
	if e != nil || !got.Exact || got.UnreadCount != 0 {
		t.Fatal("cleared counted", got, e)
	}
	// More than 10000 inspected candidates must report only a proved lower bound.
	run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id)
 SELECT $1,$2,seq,$3,$4,('0199f04a-0000-7000-8000-'||lpad(to_hex(seq),12,'0'))::uuid,'未读',decode(repeat('ab',32),'hex'),$5,$6,$7,$8,$8 FROM generate_series(4,10004) seq`, tenantA, directA, adminA, adminM, at, personA, targetM2, orgA)
	run(t, c, "UPDATE conversations SET last_seq=10004 WHERE id=$1", directA)
	got, e = s.GetUnread(ctx, groupMemberIdentity(), device, directA)
	if e != nil || got.Exact || got.UnreadCount < 1 || got.UnreadCount > 9997 || got.HeadSeq != 10004 {
		t.Fatal("unbounded or fabricated count", got, e)
	}
	s.Now = func() time.Time { return at.Add(366 * 24 * time.Hour) }
	// Lease is independently bounded; a long-expired device cannot query history.
	if _, e = s.GetUnread(ctx, groupMemberIdentity(), device, directA); e != policystore.ErrInvalidDevice {
		t.Fatal(e)
	}
}

// Inject delay into the actual PostgreSQL page query, not a client-side sleep.
type slowUnreadDB struct {
	access.Beginner
	afterFirstPage bool
}

func (d slowUnreadDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return slowUnreadTx{Tx: tx, afterFirstPage: d.afterFirstPage}, nil
}

type slowUnreadTx struct {
	pgx.Tx
	afterFirstPage bool
}

func (tx slowUnreadTx) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	if strings.Contains(q, "FROM (SELECT * FROM messages m WHERE") && (!tx.afterFirstPage || args[2].(int64) > 0) {
		q = strings.Replace(q, "FROM (SELECT * FROM messages m WHERE", "FROM (SELECT m.* FROM messages m CROSS JOIN (SELECT pg_sleep(4)) unread_pause WHERE", 1)
	}
	return tx.Tx.Query(ctx, q, args...)
}
func TestUnreadSQLBudgetReturnsOnlyProvedLowerBound(t *testing.T) {
	_, s, d := receiptDirectFixture(t)
	s.DB = slowUnreadDB{Beginner: s.DB}
	start := time.Now()
	u, e := s.GetUnread(context.Background(), groupMemberIdentity(), d, directA)
	if e != nil || u.Exact || u.UnreadCount != 0 || time.Since(start) > 3800*time.Millisecond {
		t.Fatal("scan budget not enforced", u, e, time.Since(start))
	}
}

func TestUnreadSQLBudgetRetainsCompletedPagePrefix(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id)
 SELECT $1,$2,seq,$3,$4,('0199f04a-0000-7000-8000-'||lpad(to_hex(seq),12,'0'))::uuid,'未读',decode(repeat('ab',32),'hex'),$5,$6,$7,$8,$8 FROM generate_series(2,501) seq`, tenantA, directA, adminA, adminM, at, personA, targetM2, orgA)
	run(t, c, "UPDATE conversations SET last_seq=501 WHERE id=$1", directA)
	s.DB = slowUnreadDB{Beginner: s.DB, afterFirstPage: true}
	start := time.Now()
	u, e := s.GetUnread(context.Background(), groupMemberIdentity(), d, directA)
	if e != nil || u.Exact || u.UnreadCount != 500 || time.Since(start) > 3800*time.Millisecond {
		t.Fatal("completed prefix lost", u, e, time.Since(start))
	}
}

func TestUnreadGroupHistoricalGapActingAndRetention(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	d := receiptDeviceFor(t, s, groupMemberIdentity(), 1)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "第一条")
	interval := groupIntervalFor(t, c, group.ID, personA)
	if _, e = s.LeaveGroup(ctx, groupMemberIdentity(), group.ID, interval); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 2, adminA, adminM, "离群缺口")
	if _, e = s.InviteGroupMember(ctx, publisher(), group.ID, inviteGroupRequest(targetM2)); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 3, adminA, adminM, "第三条")
	receiptDeliver(t, s, d, group.ID)
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), d, group.ID, []int64{3}); e != nil {
		t.Fatal(e)
	}
	for _, id := range []access.TrustedIdentity{groupMemberIdentity(), deviceIdentity()} {
		u, e := s.GetUnread(ctx, id, d, group.ID)
		if e != nil || !u.Exact || u.UnreadCount != 1 {
			t.Fatal("gap or cursor used as read fact", u, e)
		}
	}
	grantPublisher(t, c)
	if _, e := (access.Service{DB: c, Now: func() time.Time { return at }}).SetRetentionPolicy(ctx, publisher(), 0, 1, "test"); e != nil {
		t.Fatal(e)
	}
	s.Now = func() time.Time { return at.Add(24 * time.Hour) }
	u, e := s.GetUnread(ctx, groupMemberIdentity(), d, group.ID)
	if e != nil || !u.Exact || u.UnreadCount != 0 {
		t.Fatal("expired body counted", u, e)
	}
}
