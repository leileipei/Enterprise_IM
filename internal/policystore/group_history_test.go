package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func insertGroupHistoryMessage(t *testing.T, conn *pgx.Conn, groupID string, seq int64,
	senderUserID, senderMembershipID, body string) {
	t.Helper()
	run(t, conn, "UPDATE conversations SET last_seq=$2 WHERE id=$1", groupID, seq)
	run(t, conn, `INSERT INTO messages
 (tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,decode(repeat('ab',32),'hex'),$8)`,
		tenantA, groupID, seq, senderUserID, senderMembershipID,
		clientUUIDv7(at, 700+int(seq)), body, at)
}

func TestPullGroupTextMessagesPreservesLeaveAndRejoinGaps(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	memberInterval := groupIntervalFor(t, conn, group.ID, personA)
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "第一条")
	insertGroupHistoryMessage(t, conn, group.ID, 2, personA, targetM2, "第二条")
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, memberInterval); err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 3, adminA, adminM, "离群缺口")
	former, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(former.Messages) != 3 || former.Messages[0].Text != "第一条" ||
		former.Messages[1].Text != "第二条" || !former.Messages[2].Redacted ||
		former.Messages[2].Text != "" || former.Messages[2].MessageID != "" ||
		former.NextAfterSeq != 3 {
		t.Fatalf("former member history: %+v %v", former, err)
	}
	joined, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(targetM2))
	if err != nil || joined.JoinSeq != 4 {
		t.Fatalf("rejoin: %+v %v", joined, err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 4, adminA, adminM, "重新入群")
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	first, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 2)
	if err != nil || len(first.Messages) != 2 || first.Messages[0].Redacted || first.Messages[1].Redacted ||
		!first.HasMore || first.NextAfterSeq != 2 {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 2, 2)
	if err != nil || len(second.Messages) != 2 || !second.Messages[0].Redacted ||
		second.Messages[0].Seq != 3 || second.Messages[0].SenderUserID != "" ||
		second.Messages[1].Redacted || second.Messages[1].Text != "重新入群" ||
		second.HasMore || second.NextAfterSeq != 4 {
		t.Fatalf("second page: %+v %v", second, err)
	}
	owner, err := svc.PullGroupTextMessages(context.Background(), publisher(), group.ID, 0, 10)
	if err != nil || len(owner.Messages) != 4 || owner.Messages[2].Redacted {
		t.Fatalf("owner history: %+v %v", owner, err)
	}
}

func TestPullGroupTextMessagesRemovedMemberAndForgedSenderAreRedacted(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	interval := groupIntervalFor(t, conn, group.ID, personA)
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "授权消息")
	if _, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, interval); err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 2, adminA, adminM, "移除后")
	insertGroupHistoryMessage(t, conn, group.ID, 3, groupUserC, groupMemberC, "未入群发送者")
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 3 || page.Messages[0].Text != "授权消息" ||
		!page.Messages[1].Redacted || !page.Messages[2].Redacted {
		t.Fatalf("removed member history: %+v %v", page, err)
	}
	owner, err := svc.PullGroupTextMessages(context.Background(), publisher(), group.ID, 0, 10)
	if err != nil || len(owner.Messages) != 3 || !owner.Messages[2].Redacted {
		t.Fatalf("invalid sender interval leaked: %+v %v", owner, err)
	}
}

func TestPullGroupTextMessagesRejectsSenderMembershipOutsideGroupInterval(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	// The same user has another valid organization membership, but joined with targetM2.
	insertGroupHistoryMessage(t, conn, group.ID, 1, personA, targetM, "错误任职正文")
	page, err := svc.PullGroupTextMessages(context.Background(), publisher(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted ||
		page.Messages[0].Text != "" || page.Messages[0].MessageID != "" {
		t.Fatalf("sender membership outside interval leaked: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesHardDenyBlocksWholeGroupPage(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "被拒绝成员正文")
	insertGroupHistoryMessage(t, conn, group.ID, 2, groupUserC, groupMemberC, "第三人正文")
	hard := policy.Rule{ID: "pair-hard", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		SourceMembershipID: targetM2, TargetMembershipID: adminM,
		EffectiveFrom: at.Add(-time.Hour), Reason: "whole group read revoked"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "whole group read revoked"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].Redacted ||
		!page.Messages[1].Redacted || page.Messages[1].Text != "" || page.NextAfterSeq != 2 {
		t.Fatalf("hard deny leaked another group member's body: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesIgnoresHardDenyForNonmember(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "群内正文")
	hard := policy.Rule{ID: "outside-hard", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		SourceMembershipID: targetM2, TargetMembershipID: groupMemberC,
		EffectiveFrom: at.Add(-time.Hour), Reason: "outside this group"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "outside this group"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Redacted || page.Messages[0].Text != "群内正文" {
		t.Fatalf("unrelated hard deny hid group body: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesRedactsExpiredBodyEvenWhenStored(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "过期正文")
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=1", at.Add(-365*24*time.Hour), group.ID)
	insertGroupHistoryMessage(t, conn, group.ID, 2, adminA, adminM, "保留期内正文")
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].Redacted ||
		page.Messages[0].Text != "" || page.Messages[1].Redacted ||
		page.Messages[1].Text != "保留期内正文" {
		t.Fatalf("retention boundary: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesKeepsExpiredBodyRedactedDuringLegalHold(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-0000000002f0',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	hold, created, err := (access.Service{DB: conn, Now: func() time.Time { return at }}).
		PlaceLegalHold(context.Background(), publisher(), group.ID,
			"00000000-0000-4000-8000-0000000002f2", "CASE-GROUP-RETENTION")
	if err != nil || !created || hold.ReleasedAt != nil {
		t.Fatalf("place group hold: %+v created=%v err=%v", hold, created, err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "过期群正文")
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=1",
		at.Add(-365*24*time.Hour), group.ID)
	insertGroupHistoryMessage(t, conn, group.ID, 2, adminA, adminM, "保留期内群正文")
	first, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 1)
	if err != nil || len(first.Messages) != 1 || !first.Messages[0].Redacted ||
		first.Messages[0].Text != "" || first.Messages[0].MessageID != "" ||
		first.Messages[0].Seq != 1 || first.NextAfterSeq != 1 || !first.HasMore {
		t.Fatalf("held group expired page: %+v %v", first, err)
	}
	second, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(),
		group.ID, first.NextAfterSeq, 1)
	if err != nil || len(second.Messages) != 1 || second.Messages[0].Redacted ||
		second.Messages[0].Text != "保留期内群正文" || second.Messages[0].Seq != 2 ||
		second.NextAfterSeq != 2 || second.HasMore {
		t.Fatalf("held group retained page: %+v %v", second, err)
	}
}

func TestPullGroupTextMessagesUsesApprovedTenantRetentionDays(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE tenants SET message_body_retention_days=730,retention_version=1,
 retention_approval_reference='CAB-730',retention_approved_by_user_id=$2,retention_approved_at=$3
 WHERE id=$1`, tenantA, adminA, at)
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "approved group history")
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", at.Add(-400*24*time.Hour), group.ID)
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Redacted ||
		page.Messages[0].Text != "approved group history" {
		t.Fatalf("approved 730-day group retention: %+v %v", page, err)
	}
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", at.Add(-730*24*time.Hour), group.ID)
	page, err = svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted ||
		page.Messages[0].Text != "" || page.Messages[0].Seq != 1 {
		t.Fatalf("approved 730-day group boundary: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesKeepsOrdinaryHistoryButAppliesHardDeny(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "旧正文")
	isolate := policy.Rule{ID: "ordinary", TenantID: tenantA, Effect: policy.EffectIsolate,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		EffectiveFrom: at.Add(-time.Hour), Reason: "ordinary"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolate}, "ordinary"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Redacted || page.Messages[0].Text != "旧正文" {
		t.Fatalf("ordinary rule erased history: %+v %v", page, err)
	}
	hard := isolate
	hard.ID = "hard"
	hard.Effect = policy.EffectHardDeny
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{hard}, "hard"); err != nil {
		t.Fatal(err)
	}
	page, err = svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted || page.Messages[0].Text != "" {
		t.Fatalf("hard deny leaked group body: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesRechecksPolicyTimeAndActorExpiry(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := writer.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "临界消息")
	hard := policy.Rule{ID: "future-hard", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		EffectiveFrom: at.Add(time.Second), Reason: "future"}
	if _, err := writer.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "future"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	puller := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls > 1 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	page, err := puller.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted || calls < 2 {
		t.Fatalf("future hard deny missed: %+v %v calls=%d", page, err, calls)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), targetM2)
	calls = 0
	if page, err := puller.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10); !errors.Is(err, policystore.ErrForbidden) || len(page.Messages) != 0 {
		t.Fatalf("expired actor received group history: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesUsesHistoricalAndCurrentMembershipAfterTransfer(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "调动前历史")
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at, targetM2)
	current := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM}
	page, err := svc.PullGroupTextMessages(context.Background(), current, group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Redacted || page.Messages[0].Text != "调动前历史" {
		t.Fatalf("transfer lost old authorized history: %+v %v", page, err)
	}
	hard := policy.Rule{ID: "current-hard", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA2, TargetOrganizationID: orgA,
		EffectiveFrom: at.Add(-time.Hour), Reason: "current boundary"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "current boundary"); err != nil {
		t.Fatal(err)
	}
	page, err = svc.PullGroupTextMessages(context.Background(), current, group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted {
		t.Fatalf("current hard deny leaked transferred user's history: %+v %v", page, err)
	}
}

func TestPullGroupTextMessagesRejectsOutsidersAndAuditFailure(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, adminA, adminM, "秘密")
	outsider := access.TrustedIdentity{TenantID: tenantA, UserID: groupUserC, ActingMembershipID: groupMemberC}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	for _, id := range []access.TrustedIdentity{outsider, foreign} {
		if _, err := svc.PullGroupTextMessages(context.Background(), id, group.ID, 0, 10); !errors.Is(err, policystore.ErrMessageNotAvailable) {
			t.Fatalf("outsider history visible: %+v %v", id, err)
		}
	}
	if _, err := svc.PullGroupTextMessages(context.Background(), publisher(), group.ID, -1, 10); !errors.Is(err, policystore.ErrInvalidMessageRequest) {
		t.Fatalf("negative cursor accepted: %v", err)
	}
	if _, err := svc.PullGroupTextMessages(context.Background(), publisher(), group.ID, 0, 501); !errors.Is(err, policystore.ErrInvalidMessageRequest) {
		t.Fatalf("oversize page accepted: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	if _, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen group member read history: %v", err)
	}
	run(t, conn, "UPDATE users SET status='active' WHERE id=$1", personA)
	run(t, conn, `CREATE FUNCTION fail_group_pull_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='message_pull' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_group_pull_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_group_pull_audit()`)
	if page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10); !errors.Is(err, policystore.ErrAuditUnavailable) || len(page.Messages) != 0 {
		t.Fatalf("audit failure returned group body: %+v %v", page, err)
	}
}
