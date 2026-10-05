package policystore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestPullTextMessagesPagesForBothParticipantsAndPreservesReselectedHistory(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for i := 1; i <= 3; i++ {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 100+i), "text"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := svc.PullTextMessages(context.Background(), publisher(), directA, 0, 2)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Seq != 1 || page.Messages[1].Seq != 2 ||
		page.Messages[0].Text != "text" || page.Messages[0].MessageID == "" || !page.HasMore || page.NextAfterSeq != 2 {
		t.Fatalf("first page: %+v %v", page, err)
	}
	page, err = svc.PullTextMessages(context.Background(), publisher(), directA, 2, 2)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Seq != 3 || page.HasMore || page.NextAfterSeq != 3 {
		t.Fatalf("last page: %+v %v", page, err)
	}
	run(t, conn, "UPDATE conversations SET direct_high_membership_id=$1 WHERE id=$2", targetM, directA)
	other := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM}
	page, err = svc.PullTextMessages(context.Background(), other, directA, 0, 10)
	if err != nil || len(page.Messages) != 3 || page.Messages[0].Redacted || page.Messages[0].Text != "text" {
		t.Fatalf("reselected recipient lost history: %+v %v", page, err)
	}
	page, err = svc.PullTextMessages(context.Background(), other, directA, 999, 10)
	if err != nil || len(page.Messages) != 0 || page.NextAfterSeq != 999 || page.HasMore {
		t.Fatalf("empty page: %+v %v", page, err)
	}
	other.UserID = strings.ToUpper(other.UserID)
	other.ActingMembershipID = strings.ToUpper(other.ActingMembershipID)
	other.TenantID = strings.ToUpper(other.TenantID)
	page, err = svc.PullTextMessages(context.Background(), other, directA, 0, 10)
	if err != nil || len(page.Messages) != 3 || page.Messages[0].Redacted {
		t.Fatalf("uppercase UUID recipient lost history: %+v %v", page, err)
	}
}

func TestPullTextMessagesRedactsExpiredBodyWithoutSkippingSequence(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for i := 1; i <= 2; i++ {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA,
			clientUUIDv7(at, 200+i), "retained body"); err != nil {
			t.Fatal(err)
		}
	}
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=1",
		at.Add(-365*24*time.Hour), directA)
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2 AND seq=2",
		at.Add(-365*24*time.Hour+time.Second), directA)
	for _, reader := range []access.TrustedIdentity{
		publisher(),
		{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM2},
	} {
		page, err := svc.PullTextMessages(context.Background(), reader, directA, 0, 1)
		if err != nil || len(page.Messages) != 1 || page.Messages[0].Seq != 1 ||
			!page.Messages[0].Redacted || page.Messages[0].MessageID != "" ||
			page.Messages[0].SenderUserID != "" || page.Messages[0].Text != "" ||
			!page.Messages[0].ServerTime.IsZero() || !page.HasMore || page.NextAfterSeq != 1 {
			t.Fatalf("expired first page: %+v %v", page, err)
		}
		page, err = svc.PullTextMessages(context.Background(), reader, directA, page.NextAfterSeq, 1)
		if err != nil || len(page.Messages) != 1 || page.Messages[0].Seq != 2 ||
			page.Messages[0].Redacted || page.Messages[0].Text != "retained body" ||
			page.HasMore || page.NextAfterSeq != 2 {
			t.Fatalf("retained second page: %+v %v", page, err)
		}
	}
}

func TestPullTextMessagesKeepsExpiredBodyRedactedDuringLegalHold(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	run(t, conn, `INSERT INTO admin_grants
 (id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-0000000002f0',$1,$2,$3,'group_admin','2020-01-01')`, tenantA, adminM, orgA)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	holdService := access.Service{DB: conn, Now: func() time.Time { return at }}
	hold, created, err := holdService.PlaceLegalHold(context.Background(), admin, directA,
		"00000000-0000-4000-8000-0000000002f1", "CASE-RETENTION")
	if err != nil || !created || hold.ReleasedAt != nil {
		t.Fatalf("place hold: %+v created=%v err=%v", hold, created, err)
	}
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA,
		clientUUIDv7(at, 250), "body under hold"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2`,
		at.Add(-365*24*time.Hour), directA)
	page, err := svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted ||
		page.Messages[0].Seq != 1 || page.Messages[0].Text != "" || page.Messages[0].MessageID != "" {
		t.Fatalf("legal hold revealed expired body: %+v %v", page, err)
	}
}

func TestPullTextMessagesUsesApprovedTenantRetentionDays(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	run(t, conn, `UPDATE tenants SET message_body_retention_days=730,retention_version=1,
 retention_approval_reference='CAB-730',retention_approved_by_user_id=$2,retention_approved_at=$3
 WHERE id=$1`, tenantA, adminA, at)
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA,
		clientUUIDv7(at, 240), "approved history"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", at.Add(-400*24*time.Hour), directA)
	page, err := svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Redacted ||
		page.Messages[0].Text != "approved history" {
		t.Fatalf("approved 730-day retention: %+v %v", page, err)
	}
	run(t, conn, "UPDATE messages SET accepted_at=$1 WHERE conversation_id=$2", at.Add(-730*24*time.Hour), directA)
	page, err = svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted ||
		page.Messages[0].Text != "" || page.Messages[0].Seq != 1 {
		t.Fatalf("approved 730-day boundary: %+v %v", page, err)
	}
}

func TestPullTextMessagesRedactsLegacyAndHardDeniedButNotOrdinaryIsolation(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 110), "old authorized"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `UPDATE conversations SET last_seq=2 WHERE id=$1`, directA)
	run(t, conn, `INSERT INTO messages
 (id,tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest)
 VALUES ($1,$2,$3,2,$4,$5,$6,'legacy secret',decode(repeat('ab',32),'hex'))`,
		messageB, tenantA, directA, adminA, adminM, clientB)
	isolate := policy.Rule{ID: "ordinary", TenantID: tenantA, Effect: policy.EffectIsolate,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		EffectiveFrom: at.Add(-time.Hour), Reason: "ordinary"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolate}, "ordinary"); err != nil {
		t.Fatal(err)
	}
	page, err := svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Redacted || page.Messages[0].Text != "old authorized" ||
		!page.Messages[1].Redacted || page.Messages[1].Text != "" || page.Messages[1].MessageID != "" {
		t.Fatalf("ordinary/legacy filtering: %+v %v", page, err)
	}
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','historical-new','新组织')", directOrgC, tenantA, legalA)
	run(t, conn, "UPDATE user_organizations SET organization_id=$1 WHERE id=$2", directOrgC, targetM2)
	hard := isolate
	hard.ID = "hard"
	hard.Effect = policy.EffectHardDeny
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{hard}, "hard"); err != nil {
		t.Fatal(err)
	}
	page, err = svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].Redacted || page.Messages[0].Text != "" {
		t.Fatalf("hard deny leaked body after historical organization changed: %+v %v", page, err)
	}
}

func TestPullTextMessagesRechecksTimeAfterLocksAndBeforeReturningBody(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	grantPublisher(t, conn)
	writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := writer.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 121), "secret"); err != nil {
		t.Fatal(err)
	}
	hard := policy.Rule{ID: "future-hard", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		EffectiveFrom: at.Add(time.Second), Reason: "future"}
	if _, err := writer.Publish(context.Background(), publisher(), 0, []policy.Rule{hard}, "future"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	puller := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls >= 3 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	page, err := puller.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].Redacted || page.Messages[0].Text != "" || calls < 3 {
		t.Fatalf("effective hard deny missed after wait: %+v %v calls=%d", page, err, calls)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
	calls = 0
	if _, err := puller.PullTextMessages(context.Background(), publisher(), directA, 0, 10); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("membership expired during read: %v", err)
	}
}

func TestPullTextMessagesIdentityTenantAndAuditFailures(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	thirdUser := "00000000-0000-4000-8000-000000000261"
	thirdMember := "00000000-0000-4000-8000-000000000262"
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','第三方')", thirdUser, tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", thirdMember, tenantA, thirdUser, orgA)
	third := access.TrustedIdentity{TenantID: tenantA, UserID: thirdUser, ActingMembershipID: thirdMember}
	if _, err := svc.PullTextMessages(context.Background(), third, directA, 0, 10); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("same-tenant third party: %v", err)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err := svc.PullTextMessages(context.Background(), foreign, directA, 0, 10); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("cross tenant: %v", err)
	}
	if _, err := svc.PullTextMessages(context.Background(), publisher(), "bad", 0, 10); !errors.Is(err, policystore.ErrInvalidMessageRequest) {
		t.Fatalf("invalid id: %v", err)
	}
	if _, err := svc.PullTextMessages(context.Background(), publisher(), directA, -1, 10); !errors.Is(err, policystore.ErrInvalidMessageRequest) {
		t.Fatalf("invalid cursor: %v", err)
	}
	if _, err := svc.PullTextMessages(context.Background(), publisher(), directA, 0, 501); !errors.Is(err, policystore.ErrInvalidMessageRequest) {
		t.Fatalf("invalid limit: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	for _, conversationID := range []string{directA, directB} {
		if _, err := svc.PullTextMessages(context.Background(), publisher(), conversationID, 0, 10); !errors.Is(err, policystore.ErrForbidden) {
			t.Fatalf("frozen actor probed %s: %v", conversationID, err)
		}
	}
	run(t, conn, "UPDATE users SET status='active' WHERE id=$1", adminA)
	run(t, conn, `CREATE FUNCTION fail_message_pull_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	run(t, conn, "CREATE TRIGGER fail_message_pull_audit BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'message_pull') EXECUTE FUNCTION fail_message_pull_audit()")
	if page, err := svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10); !errors.Is(err, policystore.ErrAuditUnavailable) || len(page.Messages) != 0 {
		t.Fatalf("audit failure returned messages: %+v %v", page, err)
	}
}
