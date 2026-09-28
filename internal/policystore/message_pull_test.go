package policystore_test

import (
	"context"
	"errors"
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
	hard := isolate
	hard.ID = "hard"
	hard.Effect = policy.EffectHardDeny
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{hard}, "hard"); err != nil {
		t.Fatal(err)
	}
	page, err = svc.PullTextMessages(context.Background(), publisher(), directA, 0, 10)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].Redacted || page.Messages[0].Text != "" {
		t.Fatalf("hard deny leaked body: %+v %v", page, err)
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
