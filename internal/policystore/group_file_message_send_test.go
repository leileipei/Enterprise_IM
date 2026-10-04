package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"sync/atomic"
	"testing"
	"time"
)

func groupFileFixture(t *testing.T) (*pgx.Conn, policystore.Service, string, files.Metadata, policystore.MessageSendRequest) {
	t.Helper()
	c := fileMessageDB(t)
	seed(t, c)
	seedThirdGroupMember(t, c)
	runtimePolicyEdit(t, c)
	s := policystore.Service{DB: c, Now: func() time.Time { return at }}
	g, e := s.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if e != nil {
		t.Fatal(e)
	}
	m := fileMessageFixture(t, c, g.ID, adminA, adminM)
	return c, s, g.ID, m, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 8201), MessageType: "file", FileID: m.ID, Caption: "group file"}
}
func TestFileMessageGroupAtomic(t *testing.T) {
	c, s, cid, _, req := groupFileFixture(t)
	ack, e := s.SendGroupMessage(context.Background(), publisher(), cid, req)
	if e != nil || ack.Seq != 1 || ack.MessageID == "" || ack.ConversationID != cid || ack.Duplicate || !ack.ServerTime.Equal(at) {
		t.Fatal(ack, e)
	}
	assertFileMessageWrites(t, c, cid, 1)
	var kind, caption string
	if e = c.QueryRow(context.Background(), "SELECT message_type,text_body FROM messages WHERE id=$1", ack.MessageID).Scan(&kind, &caption); e != nil || kind != "file" || caption != req.Caption {
		t.Fatal(kind, caption, e)
	}
	var decisions, success int
	if e = c.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='send_message' AND allowed").Scan(&decisions); e != nil || decisions != 2 {
		t.Fatal(decisions, e)
	}
	if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&success); e != nil || success != 1 {
		t.Fatal(success, e)
	}
}
func TestFileMessageGroupReplay(t *testing.T) {
	c, s, cid, m, req := groupFileFixture(t)
	first, e := s.SendGroupMessage(context.Background(), publisher(), cid, req)
	if e != nil {
		t.Fatal(e)
	}
	replay := func() {
		t.Helper()
		ack, e := s.SendGroupMessage(context.Background(), publisher(), cid, req)
		if e != nil || !ack.Duplicate || ack.MessageID != first.MessageID || ack.Seq != first.Seq || !ack.ServerTime.Equal(first.ServerTime) {
			t.Fatal(first, ack, e)
		}
		assertFileMessageWrites(t, c, cid, 1)
	}
	replay()
	changed := req
	changed.Caption = "changed"
	if _, e = s.SendGroupMessage(context.Background(), publisher(), cid, changed); !errors.Is(e, policystore.ErrIdempotencyConflict) {
		t.Fatal(e)
	}
	if _, e = s.SendGroupTextMessage(context.Background(), publisher(), cid, req.ClientMessageID, req.Caption); !errors.Is(e, policystore.ErrIdempotencyConflict) {
		t.Fatal("file to text", e)
	}
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", first.MessageID, at)
	m = fileNext(m, "delete_pending")
	if e = writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	m = fileNext(m, "deleted")
	if e = writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1", tenantA)
	run(t, c, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", cid)
	replay()
}
func TestFileMessageGroupMembership(t *testing.T) {
	for _, which := range []string{"never joined", "wrong source", "left", "removed", "blocked outsider", "blocked current", "ended current", "wrong file source", "former replay", "audit failure"} {
		t.Run(which, func(t *testing.T) {
			c, s, cid, _, req := groupFileFixture(t)
			id := publisher()
			want := policystore.ErrMessageNotAvailable
			count := 0
			member := groupMemberIdentity()
			switch which {
			case "never joined", "blocked outsider":
				user, mid := freshFile().ID, freshFile().ID
				run(t, c, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES($1,$2,'outsider','outside')", user, tenantA)
				run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", mid, tenantA, user, orgA)
				id = access.TrustedIdentity{TenantID: tenantA, UserID: user, ActingMembershipID: mid}
				if which == "blocked outsider" {
					run(t, c, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", cid)
				}
			case "wrong source":
				id = member
				id.ActingMembershipID = targetM
				want = policystore.ErrConversationContextChanged
			case "left", "removed", "former replay":
				id = member
				req.FileID = fileMessageFixture(t, c, cid, personA, targetM2).ID
				if which == "former replay" {
					if _, e := s.SendGroupMessage(context.Background(), id, cid, req); e != nil {
						t.Fatal(e)
					}
					count = 1
				}
				own, e := s.GetOwnGroupMembership(context.Background(), id, cid)
				if e != nil {
					t.Fatal(e)
				}
				if which == "removed" {
					_, e = s.RemoveGroupMember(context.Background(), publisher(), cid, own.IntervalID)
				} else {
					_, e = s.LeaveGroup(context.Background(), id, cid, own.IntervalID)
				}
				if e != nil {
					t.Fatal(e)
				}
			case "blocked current":
				run(t, c, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", cid)
				want = policystore.ErrGroupPolicyBlocked
			case "ended current":
				run(t, c, "UPDATE conversations SET status='ended' WHERE id=$1", cid)
			case "wrong file source":
				req.FileID = fileMessageFixture(t, c, cid, personA, targetM2).ID
			case "audit failure":
				run(t, c, `CREATE FUNCTION fail_group_file_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$`)
				run(t, c, "CREATE TRIGGER fail_group_file_audit BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action='message_send') EXECUTE FUNCTION fail_group_file_audit()")
				want = policystore.ErrAuditUnavailable
			}
			ack, e := s.SendGroupMessage(context.Background(), id, cid, req)
			if !errors.Is(e, want) || ack.MessageID != "" {
				t.Fatal(which, ack, e, want)
			}
			if count == 0 {
				assertFileMessageWrites(t, c, cid, 0)
			} else {
				for _, table := range []string{"messages", "message_attachments", "message_idempotency", "outbox_events"} {
					var n int
					if e = c.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", cid).Scan(&n); e != nil || n != 1 {
						t.Fatal(table, n, e)
					}
				}
				var seq, rate int64
				if e = c.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", cid).Scan(&seq); e != nil || seq != 1 {
					t.Fatal(seq, e)
				}
				if e = c.QueryRow(context.Background(), "SELECT sent_count FROM message_rate_windows WHERE sender_user_id=$1", personA).Scan(&rate); e != nil || rate != 1 {
					t.Fatal(rate, e)
				}
			}
		})
	}
}
func TestFileMessageGroupFullPairPolicy(t *testing.T) {
	c, s, cid, _, req := groupFileFixture(t)
	grantPublisher(t, c)
	rule := policy.Rule{ID: "other-pair", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, EffectiveFrom: at.Add(-time.Hour), Reason: "other member pair"}
	if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "ordered pairs"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SendGroupMessage(context.Background(), publisher(), cid, req); !errors.Is(e, policystore.ErrGroupPolicyBlocked) {
		t.Fatal(e)
	}
	assertFileMessageWrites(t, c, cid, 0)
	var status string
	var deny int
	if e := c.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", cid).Scan(&status); e != nil || status != "policy_blocked" {
		t.Fatal(status, e)
	}
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='send_message' AND NOT allowed AND 'other-pair'=ANY(matched_rule_ids)").Scan(&deny); e != nil || deny != 1 {
		t.Fatal(deny, e)
	}
}
func TestFileMessageGroupFinalTime(t *testing.T) {
	for _, which := range []string{"actor", "other member", "rule", "replay actor"} {
		t.Run(which, func(t *testing.T) {
			c, s, cid, _, req := groupFileFixture(t)
			want := policystore.ErrGroupPolicyBlocked
			count := 0
			switch which {
			case "actor", "replay actor":
				if which == "replay actor" {
					if _, e := s.SendGroupMessage(context.Background(), publisher(), cid, req); e != nil {
						t.Fatal(e)
					}
					count = 1
				}
				run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at.Add(time.Second))
				want = policystore.ErrForbidden
			case "other member":
				run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", targetM2, at.Add(time.Second))
			case "rule":
				grantPublisher(t, c)
				rule := policy.Rule{ID: "late-stop", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "late rule"}
				if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "late rule"); e != nil {
					t.Fatal(e)
				}
			}
			var clock atomic.Int64
			clock.Store(at.UnixNano())
			s.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			s.DB = afterFileAuditDB{Beginner: c, after: func() { clock.Store(at.Add(2 * time.Second).UnixNano()) }}
			ack, e := s.SendGroupMessage(context.Background(), publisher(), cid, req)
			if !errors.Is(e, want) || ack.MessageID != "" {
				t.Fatal(which, ack, e, want)
			}
			assertFileMessageWrites(t, c, cid, count)
			var success int
			if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&success); e != nil || success != count {
				t.Fatal(success, count, e)
			}
			var status string
			if e = c.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", cid).Scan(&status); e != nil {
				t.Fatal(e)
			}
			if want == policystore.ErrGroupPolicyBlocked && status != "policy_blocked" {
				t.Fatal("denial evidence missing", status)
			}
		})
	}
}

func TestFileMessageGroupReplayTextConflict(t *testing.T) {
	c, s, cid, _, req := groupFileFixture(t)
	first, e := s.SendGroupTextMessage(context.Background(), publisher(), cid, req.ClientMessageID, "old text")
	if e != nil {
		t.Fatal(e)
	}
	ack, e := s.SendGroupMessage(context.Background(), publisher(), cid, req)
	if !errors.Is(e, policystore.ErrIdempotencyConflict) || ack.MessageID != "" {
		t.Fatal(ack, e)
	}
	var seq, links int64
	if e = c.QueryRow(context.Background(), "SELECT last_seq,(SELECT count(*) FROM message_attachments WHERE conversation_id=$1) FROM conversations WHERE id=$1", cid).Scan(&seq, &links); e != nil || seq != first.Seq || links != 0 {
		t.Fatal(seq, links, e)
	}
}
