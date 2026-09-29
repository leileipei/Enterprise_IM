package policystore_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const (
	directA       = "00000000-0000-4000-8000-000000000401"
	directB       = "00000000-0000-4000-8000-000000000402"
	directLegalC  = "00000000-0000-4000-8000-000000000411"
	directOrgC    = "00000000-0000-4000-8000-000000000421"
	directMemberC = "00000000-0000-4000-8000-000000000441"
)

func TestDirectConversationSchemaEnforcesPairAndTenant(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	insert := `INSERT INTO conversations
 (id,tenant_id,kind,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,'direct',$3,$4,$5,$6,$7)`
	run(t, conn, insert, directA, tenantA, adminA, personA, adminM, targetM2, adminA)
	for _, tc := range []struct {
		name                   string
		conversationID, tenant string
		lowUser, highUser      string
		lowMember, highMember  string
		creator                string
	}{
		{"duplicate pair", directB, tenantA, adminA, personA, adminM, targetM2, adminA},
		{"reversed pair", directB, tenantA, personA, adminA, targetM2, adminM, adminA},
		{"self chat", directB, tenantA, adminA, adminA, adminM, adminM, adminA},
		{"foreign user", directB, tenantA, adminA, personB, adminM, otherM, adminA},
		{"wrong user membership", directB, tenantA, adminA, personA, targetM2, adminM, adminA},
		{"creator outside pair", directB, tenantA, adminA, personA, adminM, targetM2, personB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := conn.Exec(context.Background(), insert, tc.conversationID, tc.tenant,
				tc.lowUser, tc.highUser, tc.lowMember, tc.highMember, tc.creator); err == nil {
				t.Fatal("invalid direct conversation accepted")
			}
		})
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE tenant_id=$1", tenantA).Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid conversation changed table: %d %v", count, err)
	}
}

func TestConversationSchemaRejectsIncompleteGroupRow(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	if _, err := conn.Exec(context.Background(), `INSERT INTO conversations
 (tenant_id,kind,created_by_user_id) VALUES ($1,'group',$2)`, tenantA, adminA); err == nil {
		t.Fatal("group conversation accepted without required metadata")
	}
}

func TestDirectConversationMigrationRollsBackAndReapplies(t *testing.T) {
	conn := db(t)
	ctx := context.Background()
	for _, path := range []string{
		"../../db/migrations/000010_group_create_request.down.sql",
		"../../db/migrations/000009_group_membership.down.sql",
		"../../db/migrations/000008_conversation_inbox.down.sql",
		"../../db/migrations/000007_message_recipient.down.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	messageDown, err := os.ReadFile("../../db/migrations/000006_message_write.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(messageDown)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../db/migrations/000005_direct_conversations.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var relation *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('conversations')::text").Scan(&relation); err != nil || relation != nil {
		t.Fatalf("conversation table after down: %v %v", relation, err)
	}
	up, err := os.ReadFile("../../db/migrations/000005_direct_conversations.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	messageUp, err := os.ReadFile("../../db/migrations/000006_message_write.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(messageUp)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"../../db/migrations/000007_message_recipient.up.sql",
		"../../db/migrations/000008_conversation_inbox.up.sql",
		"../../db/migrations/000009_group_membership.up.sql",
		"../../db/migrations/000010_group_create_request.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	seed(t, conn)
}

func TestStartDirectConversationCreatesAndReusesByUserPair(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	first, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2)
	if err != nil || first.ID == "" || first.LastSeq != 0 || first.PolicyVersion != 0 || first.CrossLegal ||
		first.DecisionReason != policy.ReasonAllowedSameOrganization {
		t.Fatalf("create same-org direct chat: %+v %v", first, err)
	}
	reverse := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM2}
	second, err := svc.StartDirectConversation(context.Background(), reverse, adminM)
	if err != nil || second.ID != first.ID {
		t.Fatalf("reverse pair not reused: %+v %v", second, err)
	}
	var count, audits int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE tenant_id=$1 AND kind='direct'", tenantA).Scan(&count); err != nil || count != 1 {
		t.Fatalf("conversation count: %d %v", count, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='conversation_start' AND outcome='allow'").Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("conversation start audits: %d %v", audits, err)
	}
}

func TestStartDirectConversationReusesAfterMembershipReselection(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	first, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2)
	if err != nil {
		t.Fatal(err)
	}
	allow := policy.Rule{ID: "chat-cross-org", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目联系",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放单聊"); err != nil {
		t.Fatal(err)
	}
	second, err := svc.StartDirectConversation(context.Background(), publisher(), targetM)
	if err != nil || second.ID != first.ID || second.PolicyVersion != 1 || second.DecisionReason != policy.ReasonAllowedRule {
		t.Fatalf("reseated membership conversation: %+v %v", second, err)
	}
	var selected string
	if err := conn.QueryRow(context.Background(), "SELECT direct_high_membership_id::text FROM conversations WHERE id=$1", first.ID).Scan(&selected); err != nil || selected != targetM {
		t.Fatalf("current selected membership: %s %v", selected, err)
	}
}

func TestStartDirectConversationReportsCrossLegalMembershipFact(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'c','法人 C')", directLegalC, tenantA)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','c','公司 C')", directOrgC, tenantA, directLegalC)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", directMemberC, tenantA, personA, directOrgC)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "cross-legal-direct", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: directOrgC,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "跨法人项目联系",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "跨法人单聊"); err != nil {
		t.Fatal(err)
	}
	chat, err := svc.StartDirectConversation(context.Background(), publisher(), directMemberC)
	if err != nil || chat.ID == "" || !chat.CrossLegal || chat.DecisionReason != policy.ReasonAllowedRule {
		t.Fatalf("cross-legal direct conversation: %+v %v", chat, err)
	}
}

func TestStartDirectConversationHidesUnavailableTargetAndSelf(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.StartDirectConversation(context.Background(), publisher(), "bad"); !errors.Is(err, policystore.ErrInvalidChatTarget) {
		t.Fatalf("invalid target accepted: %v", err)
	}
	for _, target := range []string{targetM, otherM, adminM, "00000000-0000-4000-8000-000000000499"} {
		chat, err := svc.StartDirectConversation(context.Background(), publisher(), target)
		if !errors.Is(err, policystore.ErrChatNotAvailable) || chat.ID != "" {
			t.Fatalf("unavailable target leaked: %s %+v %v", target, chat, err)
		}
	}
	var chats, denied int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations").Scan(&chats); err != nil || chats != 0 {
		t.Fatalf("denied conversation created: %d %v", chats, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='conversation_start' AND outcome='deny'").Scan(&denied); err != nil || denied != 4 {
		t.Fatalf("denied start audits: %d %v", denied, err)
	}
}

func TestStartDirectConversationExistingPairCannotBypassHardDeny(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "cross-chat", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目联系",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放单聊"); err != nil {
		t.Fatal(err)
	}
	first, err := svc.StartDirectConversation(context.Background(), publisher(), targetM)
	if err != nil || first.ID == "" {
		t.Fatalf("allowed cross-org chat: %+v %v", first, err)
	}
	hard := policy.Rule{ID: "block-chat", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "强制隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "停止单聊"); err != nil {
		t.Fatal(err)
	}
	if chat, err := svc.StartDirectConversation(context.Background(), publisher(), targetM); !errors.Is(err, policystore.ErrChatNotAvailable) || chat.ID != "" {
		t.Fatalf("old conversation ID leaked after hard deny: %+v %v", chat, err)
	}
	var version int64
	if err := conn.QueryRow(context.Background(), "SELECT last_policy_version FROM conversations WHERE id=$1", first.ID).Scan(&version); err != nil || version != 1 {
		t.Fatalf("denied retry changed conversation: %d %v", version, err)
	}
}

func TestStartDirectConversationEndedPairDoesNotReactivate(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	first, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2)
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='ended' WHERE id=$1", first.ID)
	if chat, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2); !errors.Is(err, policystore.ErrChatNotAvailable) || chat.ID != "" {
		t.Fatalf("ended conversation reactivated: %+v %v", chat, err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", first.ID).Scan(&status); err != nil || status != "ended" {
		t.Fatalf("ended status changed: %s %v", status, err)
	}
}

func TestStartDirectConversationReuseAuditFailureRollsBackMembershipChange(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	first, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2)
	if err != nil {
		t.Fatal(err)
	}
	allow := policy.Rule{ID: "chat-reselect", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目联系",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "跨组织重选任职"); err != nil {
		t.Fatal(err)
	}
	run(t, conn, "ALTER TABLE audit_events ADD CONSTRAINT reject_future_chat_audit CHECK (action <> 'conversation_start') NOT VALID")
	if chat, err := svc.StartDirectConversation(context.Background(), publisher(), targetM); !errors.Is(err, policystore.ErrAuditUnavailable) || chat.ID != "" {
		t.Fatalf("conversation returned after replay audit failure: %+v %v", chat, err)
	}
	var membershipID string
	var version int64
	if err := conn.QueryRow(context.Background(), "SELECT direct_high_membership_id::text,last_policy_version FROM conversations WHERE id=$1", first.ID).Scan(&membershipID, &version); err != nil || membershipID != targetM2 || version != 0 {
		t.Fatalf("replay audit failure changed conversation: %s %d %v", membershipID, version, err)
	}
}

func TestStartDirectConversationInvalidIdentityAndAuditFailures(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if chat, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2); !errors.Is(err, policystore.ErrForbidden) || chat.ID != "" {
		t.Fatalf("ended actor created chat: %+v %v", chat, err)
	}
	for _, tc := range []struct{ name, constraint string }{
		{"decision", "ALTER TABLE policy_decision_events ADD CONSTRAINT reject_chat_decision CHECK (action <> 'start_chat')"},
		{"request", "ALTER TABLE audit_events ADD CONSTRAINT reject_chat_request CHECK (action <> 'conversation_start')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			run(t, conn, tc.constraint)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			chat, err := svc.StartDirectConversation(context.Background(), publisher(), targetM2)
			if !errors.Is(err, policystore.ErrAuditUnavailable) || chat.ID != "" {
				t.Fatalf("conversation returned without audit: %+v %v", chat, err)
			}
			var chats, decisions int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations").Scan(&chats); err != nil || chats != 0 {
				t.Fatalf("conversation committed after audit failure: %d %v", chats, err)
			}
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='start_chat'").Scan(&decisions); err != nil || decisions != 0 {
				t.Fatalf("decision audit partially committed: %d %v", decisions, err)
			}
		})
	}
}

func TestStartDirectConversationConcurrentRequestsShareID(t *testing.T) {
	firstConn := db(t)
	seed(t, firstConn)
	ctx := context.Background()
	var searchPath string
	if err := firstConn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	secondConn, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondConn.Close(ctx) })
	if _, err := secondConn.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
		t.Fatal(err)
	}
	type result struct {
		chat policystore.DirectConversation
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, conn := range []*pgx.Conn{firstConn, secondConn} {
		go func(conn *pgx.Conn) {
			ready.Done()
			<-start
			chat, err := (policystore.Service{DB: conn, Now: func() time.Time { return at }}).
				StartDirectConversation(ctx, publisher(), targetM2)
			results <- result{chat, err}
		}(conn)
	}
	ready.Wait()
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.chat.ID == "" || a.chat.ID != b.chat.ID {
		t.Fatalf("concurrent pair not unique: %+v %+v", a, b)
	}
	var count int
	if err := firstConn.QueryRow(ctx, "SELECT count(*) FROM conversations WHERE tenant_id=$1", tenantA).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent conversation count: %d %v", count, err)
	}
}
