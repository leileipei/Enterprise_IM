package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
	"time"
)

func receiptDeviceFor(t *testing.T, s policystore.Service, id access.TrustedIdentity, n int) policystore.DeviceCredential {
	t.Helper()
	g, e := s.RegisterDevice(context.Background(), id, deviceRegistration(n))
	if e != nil {
		t.Fatal(e)
	}
	return policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}
}
func TestGroupReceiptSummaryVisibilityAndDedup(t *testing.T) {
	c := db(t)
	seed(t, c)
	seedThirdGroupMember(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2, groupMemberC))
	if e != nil {
		t.Fatal(e)
	}
	ownerDevice := receiptDeviceFor(t, s, publisher(), 1)
	memberDevice := receiptDeviceFor(t, s, groupMemberIdentity(), 2)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "owner消息")
	receiptDeliver(t, s, memberDevice, group.ID)
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), memberDevice, group.ID, []int64{1}); e != nil {
		t.Fatal(e)
	}
	second := receiptDeviceFor(t, s, groupMemberIdentity(), 3)
	receiptDeliver(t, s, second, group.ID)
	got, e := s.QueryReceipts(ctx, publisher(), ownerDevice, group.ID, []int64{1})
	if e != nil || len(got) != 1 || !got[0].SummaryAvailable || got[0].VisibleRecipientCount != 2 || got[0].VisibleDeliveredCount != 1 || got[0].VisibleReadCount != 1 {
		t.Fatal(got, e)
	}
	// Visibility is current ordinary directory policy, not knowledge of the roster.
	grantPublisher(t, c)
	rule := policy.Rule{ID: "hide-receipt-member", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionDirectoryView, SourceMembershipID: adminM, TargetMembershipID: groupMemberC, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Second), Reason: "test"}
	if _, e = s.Publish(ctx, publisher(), 0, []policy.Rule{rule}, "test"); e != nil {
		t.Fatal(e)
	}
	got, e = s.QueryReceipts(ctx, publisher(), ownerDevice, group.ID, []int64{1})
	if e != nil || got[0].VisibleRecipientCount != 1 || got[0].VisibleReadCount != 1 {
		t.Fatal("hidden candidate leaked", got, e)
	}
	// Default policy suppresses summaries for a non-admin sender.
	insertGroupHistoryMessage(t, c, group.ID, 2, personA, targetM2, "member消息")
	got, e = s.QueryReceipts(ctx, groupMemberIdentity(), memberDevice, group.ID, []int64{2})
	if e != nil || got[0].SummaryAvailable || got[0].Status != "sent" || got[0].VisibleRecipientCount != 0 {
		t.Fatal(got, e)
	}
	p := policystore.ClientPolicy{DeviceLeaseDays: 30, MaxActiveDevices: 10, OfflineCacheSeconds: 300, GroupReceiptVisibility: "sender_aggregate"}
	if _, e = s.SetClientPolicy(ctx, publisher(), 0, p); e != nil {
		t.Fatal(e)
	}
	got, e = s.QueryReceipts(ctx, groupMemberIdentity(), memberDevice, group.ID, []int64{2})
	if e != nil || !got[0].SummaryAvailable || got[0].VisibleRecipientCount != 2 {
		t.Fatal("aggregate opt-in", got, e)
	}
	if _, e = s.ListReceiptUsers(ctx, groupMemberIdentity(), memberDevice, group.ID, 2, "", 20); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("member saw names", e)
	}
	if _, e = s.QueryReceipts(ctx, publisher(), ownerDevice, group.ID, []int64{1, 2}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("mixed batch leak", e)
	}
}
func TestGroupReceiptPaginationRechecksAuthorization(t *testing.T) {
	c := db(t)
	seed(t, c)
	seedThirdGroupMember(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2, groupMemberC))
	if e != nil {
		t.Fatal(e)
	}
	device := receiptDeviceFor(t, s, publisher(), 1)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "分页")
	p, e := s.ListReceiptUsers(ctx, publisher(), device, group.ID, 1, "", 1)
	if e != nil || len(p.Users) != 1 || !p.HasMore || p.NextCursor == "" {
		t.Fatal(p, e)
	}
	p2, e := s.ListReceiptUsers(ctx, publisher(), device, group.ID, 1, p.NextCursor, 1)
	if e != nil || len(p2.Users) != 1 || p2.HasMore || p2.Users[0].UserID == p.Users[0].UserID {
		t.Fatal(p2, e)
	}
	const adminM2 = "aaaaaaaa-0000-0000-0000-000000000099"
	other := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM2}
	// Seed another valid organization membership for the owner: changing it cannot carry the roster grant.
	run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", adminM2, tenantA, adminA, orgA2)
	if _, e = s.ListReceiptUsers(ctx, other, device, group.ID, 1, p.NextCursor, 1); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("cursor carried acting grant", e)
	}
	run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", targetM2, at)
	got, e := s.QueryReceipts(ctx, publisher(), device, group.ID, []int64{1})
	// The other organization has no directory allow: expired same-org membership is excluded.
	if e != nil || got[0].VisibleRecipientCount != 1 {
		t.Fatal(got, e)
	}
	run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE user_id=$1", personA, at)
	got, e = s.QueryReceipts(ctx, publisher(), device, group.ID, []int64{1})
	if e != nil || got[0].VisibleRecipientCount != 1 {
		t.Fatal("expired user counted", got, e)
	}
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$1", at)
	if _, e = s.ListReceiptUsers(ctx, publisher(), device, group.ID, 1, p.NextCursor, 1); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("cleared body queried", e)
	}
}
func TestReceiptDirectStatusAndNoRecipientQuery(t *testing.T) {
	_, s, recipient := receiptDirectFixture(t)
	ctx := context.Background()
	sender := receiptDeviceFor(t, s, publisher(), 2)
	got, e := s.QueryReceipts(ctx, publisher(), sender, directA, []int64{1})
	if e != nil || got[0].Status != "sent" {
		t.Fatal(got, e)
	}
	receiptDeliver(t, s, recipient, directA)
	got, e = s.QueryReceipts(ctx, publisher(), sender, directA, []int64{1})
	if e != nil || got[0].Status != "delivered" || got[0].DeliveredAt == nil {
		t.Fatal(got, e)
	}
	if _, e = s.ConfirmRead(ctx, groupMemberIdentity(), recipient, directA, []int64{1}); e != nil {
		t.Fatal(e)
	}
	got, e = s.QueryReceipts(ctx, publisher(), sender, directA, []int64{1})
	if e != nil || got[0].Status != "read" || got[0].ReadAt == nil {
		t.Fatal(got, e)
	}
	if _, e = s.QueryReceipts(ctx, groupMemberIdentity(), recipient, directA, []int64{1}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("recipient queried sender state", e)
	}
}

func TestGroupReceiptHistoricalIntervalsNeverFillRejoinGap(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	d := receiptDeviceFor(t, s, publisher(), 1)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "前区间")
	interval := groupIntervalFor(t, c, group.ID, personA)
	if _, e = s.LeaveGroup(ctx, groupMemberIdentity(), group.ID, interval); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 2, adminA, adminM, "离群缺口")
	if _, e = s.InviteGroupMember(ctx, publisher(), group.ID, inviteGroupRequest(targetM2)); e != nil {
		t.Fatal(e)
	}
	insertGroupHistoryMessage(t, c, group.ID, 3, adminA, adminM, "再入群")
	got, e := s.QueryReceipts(ctx, publisher(), d, group.ID, []int64{1, 2, 3})
	if e != nil || len(got) != 3 || got[0].VisibleRecipientCount != 1 || got[1].VisibleRecipientCount != 0 || got[2].VisibleRecipientCount != 1 {
		t.Fatal("current roster filled historical gap", got, e)
	}
}
func TestGroupReceiptFinalDirectoryTimeAndAuditFailure(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	d := receiptDeviceFor(t, s, publisher(), 1)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "时效")
	grantPublisher(t, c)
	rule := policy.Rule{ID: "future-receipt-directory-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "test"}
	if _, e = s.Publish(ctx, publisher(), 0, []policy.Rule{rule}, "test"); e != nil {
		t.Fatal(e)
	}
	clock := at
	s.Now = func() time.Time { return clock }
	s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = at.Add(time.Second) }}
	got, e := s.QueryReceipts(ctx, publisher(), d, group.ID, []int64{1})
	if e != nil || got[0].VisibleRecipientCount != 0 {
		t.Fatal("late directory deny ignored", got, e)
	}
	run(t, c, `ALTER TABLE audit_events ADD CONSTRAINT receipt_query_audit_disabled CHECK(action<>'message_receipt_query') NOT VALID`)
	if _, e = s.QueryReceipts(ctx, publisher(), d, group.ID, []int64{1}); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal("query succeeded without required audit", e)
	}
}

type afterReceiptPeerDB struct {
	access.Beginner
	after func()
}

func (d afterReceiptPeerDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return afterReceiptPeerTx{Tx: tx, after: d.after}, nil
}

type afterReceiptPeerTx struct {
	pgx.Tx
	after func()
}

func (tx afterReceiptPeerTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, q, args...)
	if strings.Contains(q, "user_id<>$3") && strings.Contains(q, "source_membership_id=$5") {
		return afterReceiptStateRow{Row: row, after: tx.after}
	}
	return row
}
func TestGroupReceiptRuleActivationDuringLastPeerQuery(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	group, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	d := receiptDeviceFor(t, s, publisher(), 1)
	insertGroupHistoryMessage(t, c, group.ID, 1, adminA, adminM, "最后查询边界")
	grantPublisher(t, c)
	rules := []policy.Rule{{ID: "no-matching-peer", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA2, EffectiveFrom: at.Add(-time.Second), Reason: "test"}, {ID: "future-group-receipt-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "test"}}
	if _, e = s.Publish(ctx, publisher(), 0, rules, "test"); e != nil {
		t.Fatal(e)
	}
	clock := at
	armed := false
	s.Now = func() time.Time { return clock }
	s.DB = afterReceiptPeerDB{Beginner: afterDeviceAuditDB{Beginner: c, after: func() { armed = true }}, after: func() {
		if armed {
			clock = at.Add(time.Second)
		}
	}}
	if _, e = s.QueryReceipts(ctx, publisher(), d, group.ID, []int64{1}); !errors.Is(e, policystore.ErrReceiptNotAvailable) {
		t.Fatal("policy activated during final query but receipt leaked", e)
	}
}
