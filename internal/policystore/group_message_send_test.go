package policystore_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestSendGroupTextMessageCommitsOnceAndPullsForMembers(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	clientID := clientUUIDv7(at, 801)
	first, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID, clientID, "群内你好")
	if err != nil || first.MessageID == "" || first.ConversationID != group.ID ||
		first.Seq != 1 || !first.ServerTime.Equal(at) || first.Duplicate {
		t.Fatalf("first group ACK: %+v %v", first, err)
	}
	replay, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID, clientID, "群内你好")
	if err != nil || replay.MessageID != first.MessageID || replay.Seq != first.Seq ||
		!replay.ServerTime.Equal(first.ServerTime) || !replay.Duplicate {
		t.Fatalf("replay group ACK: %+v %+v %v", first, replay, err)
	}
	for table, want := range map[string]int{"messages": 1, "message_idempotency": 1, "outbox_events": 1} {
		var count int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", group.ID).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count: %d %v", table, count, err)
		}
	}
	var decisions int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM policy_decision_events
 WHERE action='send_message' AND allowed AND actor_user_id=$1`, adminA).Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("group send allow decision audit: %d %v", decisions, err)
	}
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Redacted || page.Messages[0].Text != "群内你好" {
		t.Fatalf("member group pull: %+v %v", page, err)
	}
	if ack, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID, clientID, "不同正文"); !errors.Is(err, policystore.ErrIdempotencyConflict) || ack.MessageID != "" {
		t.Fatalf("changed payload reused key: %+v %v", ack, err)
	}
}

func TestSendGroupTextMessageRequiresCurrentSourceMembership(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	wrong := groupMemberIdentity()
	wrong.ActingMembershipID = targetM
	if _, err := svc.SendGroupTextMessage(context.Background(), wrong, group.ID, clientUUIDv7(at, 802), "错误任职"); !errors.Is(err, policystore.ErrConversationContextChanged) {
		t.Fatalf("wrong source membership sent: %v", err)
	}
	outsider := access.TrustedIdentity{TenantID: tenantA, UserID: groupUserC, ActingMembershipID: groupMemberC}
	if _, err := svc.SendGroupTextMessage(context.Background(), outsider, group.ID, clientUUIDv7(at, 803), "非群成员"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("outsider sent: %v", err)
	}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 804), "错误会话"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("direct conversation accepted as group: %v", err)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err := svc.SendGroupTextMessage(context.Background(), foreign, group.ID, clientUUIDv7(at, 805), "跨租户"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("foreign tenant sent: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(-time.Second), targetM2)
	if _, err := svc.SendGroupTextMessage(context.Background(), outsider, group.ID, clientUUIDv7(at, 811), "探测失效成员"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("outsider changed unavailable group: %v", err)
	}
	var status string
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "active" {
		t.Fatalf("invalid caller changed group status: %s %v", status, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE conversation_id=$1", group.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid send persisted: %d %v", count, err)
	}
}

func TestSendGroupTextMessageOutsiderCannotProbeBlockedGroup(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	outsider := access.TrustedIdentity{TenantID: tenantA, UserID: groupUserC, ActingMembershipID: groupMemberC}
	if _, err := svc.SendGroupTextMessage(context.Background(), outsider, group.ID,
		clientUUIDv7(at, 825), "探测群状态"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("outsider learned blocked status: %v", err)
	}
}

func TestSendGroupTextMessageRechecksTimeAfterOutboxWrite(t *testing.T) {
	for _, expired := range []string{"policy", "actor"} {
		t.Run(expired, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			group, err := writer.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
			if err != nil {
				t.Fatal(err)
			}
			if expired == "policy" {
				grantPublisher(t, conn)
				rule := policy.Rule{ID: "late-stop", TenantID: tenantA, Effect: policy.EffectHardDeny,
					Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
					EffectiveFrom: at.Add(time.Second), Reason: "late stop"}
				if _, err := writer.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "late stop"); err != nil {
					t.Fatal(err)
				}
			} else {
				run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
			}
			calls := 0
			svc := policystore.Service{DB: conn, Now: func() time.Time {
				calls++
				if calls >= 5 {
					return at.Add(2 * time.Second)
				}
				return at
			}}
			_, err = svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
				clientUUIDv7(at, 826), "临界提交")
			if expired == "policy" && !errors.Is(err, policystore.ErrGroupPolicyBlocked) ||
				expired == "actor" && !errors.Is(err, policystore.ErrForbidden) {
				t.Fatalf("late %s expiry accepted: %v calls=%d", expired, err, calls)
			}
			var status string
			var seq, messages, outboxes, reservations int
			if err := conn.QueryRow(context.Background(), "SELECT status,last_seq FROM conversations WHERE id=$1", group.ID).Scan(&status, &seq); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				query string
				out   *int
				arg   string
			}{
				{"SELECT count(*) FROM messages WHERE conversation_id=$1", &messages, group.ID},
				{"SELECT count(*) FROM outbox_events WHERE conversation_id=$1", &outboxes, group.ID},
				{"SELECT count(*) FROM message_rate_windows WHERE tenant_id=$1", &reservations, tenantA},
			} {
				if err := conn.QueryRow(context.Background(), item.query, item.arg).Scan(item.out); err != nil {
					t.Fatal(err)
				}
			}
			wantStatus := "active"
			if expired == "policy" {
				wantStatus = "policy_blocked"
			}
			if status != wantStatus || seq != 0 || messages != 0 || outboxes != 0 || reservations != 0 {
				t.Fatalf("late %s expiry left writes: %s seq=%d messages=%d outbox=%d rate=%d", expired, status, seq, messages, outboxes, reservations)
			}
		})
	}
}

func TestSendGroupTextMessageRateLockWaitCrossesPolicyBoundary(t *testing.T) {
	for _, variant := range []struct {
		name  string
		limit int
	}{{"accepted_rate", 10}, {"rejected_rate", 1}} {
		t.Run(variant.name, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			grantPublisher(t, conn)
			writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			group, err := writer.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
			if err != nil {
				t.Fatal(err)
			}
			rule := policy.Rule{ID: "wait-stop", TenantID: tenantA, Effect: policy.EffectHardDeny,
				Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
				EffectiveFrom: at.Add(time.Second), Reason: "wait stop"}
			if _, err := writer.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "wait stop"); err != nil {
				t.Fatal(err)
			}
			run(t, conn, `INSERT INTO message_rate_windows
 (tenant_id,sender_user_id,window_start,sent_count) VALUES ($1,$2,date_trunc('second',$3::timestamptz),1)`, tenantA, adminA, at)
			var schema string
			if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
				t.Fatal(err)
			}
			open := func() *pgx.Conn {
				c, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close(context.Background()) })
				return c
			}
			blocker, sender := open(), open()
			var senderPID int
			if err := sender.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&senderPID); err != nil {
				t.Fatal(err)
			}
			blockTx, err := blocker.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer blockTx.Rollback(context.Background())
			if _, err := blockTx.Exec(context.Background(), `UPDATE message_rate_windows SET sent_count=1
 WHERE tenant_id=$1 AND sender_user_id=$2`, tenantA, adminA); err != nil {
				t.Fatal(err)
			}
			var later atomic.Bool
			done := make(chan error, 1)
			go func() {
				svc := policystore.Service{DB: sender, MessageRatePerSecond: variant.limit, Now: func() time.Time {
					if later.Load() {
						return at.Add(2 * time.Second)
					}
					return at
				}}
				_, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
					clientUUIDv7(at, 827), "等待限流锁")
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				var waitType *string
				if err := conn.QueryRow(context.Background(), "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", senderPID).Scan(&waitType); err != nil {
					t.Fatal(err)
				}
				if waitType != nil && *waitType == "Lock" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("sender did not wait on rate row")
				}
				time.Sleep(10 * time.Millisecond)
			}
			later.Store(true)
			if err := blockTx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
					t.Fatalf("policy boundary passed while waiting on rate lock: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("sender did not finish after rate lock released")
			}
			var status string
			var seq, messages int
			if err := conn.QueryRow(context.Background(), "SELECT status,last_seq FROM conversations WHERE id=$1", group.ID).Scan(&status, &seq); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE conversation_id=$1", group.ID).Scan(&messages); err != nil {
				t.Fatal(err)
			}
			if status != "policy_blocked" || seq != 0 || messages != 0 {
				t.Fatalf("lock wait send persisted: status=%s seq=%d messages=%d", status, seq, messages)
			}
		})
	}
}

func TestSendGroupTextMessage500Members(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO users (id,tenant_id,global_employee_no,display_name)
 SELECT ('00000000-0000-4000-8000-'||lpad(to_hex(10000+n),12,'0'))::uuid,
 $1,'G'||n,'群成员'||n FROM generate_series(1,498) AS n`, tenantA)
	run(t, conn, `INSERT INTO user_organizations
 (id,tenant_id,user_id,organization_id,effective_from)
 SELECT ('00000000-0000-4000-8000-'||lpad(to_hex(20000+n),12,'0'))::uuid,
 $1,('00000000-0000-4000-8000-'||lpad(to_hex(10000+n),12,'0'))::uuid,
 $2,'2020-01-01'::timestamptz FROM generate_series(1,498) AS n`, tenantA, orgA)
	run(t, conn, `INSERT INTO conversation_membership_intervals
 (tenant_id,conversation_id,user_id,source_membership_id,source_organization_id,
  source_legal_entity_id,join_seq,joined_policy_version)
 SELECT $1,$2,('00000000-0000-4000-8000-'||lpad(to_hex(10000+n),12,'0'))::uuid,
 ('00000000-0000-4000-8000-'||lpad(to_hex(20000+n),12,'0'))::uuid,
 $3,$4,1,0 FROM generate_series(1,498) AS n`, tenantA, group.ID, orgA, legalA)
	start := time.Now()
	ack, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 828), "五百人群")
	t.Logf("500-member send transaction: %s", time.Since(start))
	if err != nil || ack.Seq != 1 {
		t.Fatalf("500-member send: %+v %v", ack, err)
	}
	var decisions int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM policy_decision_events
 WHERE action='send_message' AND allowed AND actor_user_id=$1`, adminA).Scan(&decisions); err != nil || decisions != 499 {
		t.Fatalf("500-member decision audit: %d %v", decisions, err)
	}
	grantPublisher(t, conn)
	rule := policy.Rule{ID: "member-scoped-future", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		SourceMembershipID: adminM, TargetMembershipID: targetM2,
		EffectiveFrom: at.Add(time.Hour), Reason: "future member deny"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "future member rule"); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	ack, err = svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 830), "五百人群逐任职规则")
	t.Logf("500-member send with membership-scoped rule: %s", time.Since(start))
	if err != nil || ack.Seq != 2 {
		t.Fatalf("500-member rule send: %+v %v", ack, err)
	}
}

func TestSendGroupTextMessageRetriesMembershipLockCycle(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := writer.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	open := func(deadlockTimeout string) *pgx.Conn {
		c, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(context.Background(), "SET deadlock_timeout='"+deadlockTimeout+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	sender, blocker := open("100ms"), open("5s")
	var blockerPID int
	if err := blocker.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	releaseSender := make(chan struct{})
	var calls atomic.Int32
	sendDone := make(chan error, 1)
	go func() {
		svc := policystore.Service{DB: sender, Now: func() time.Time {
			if calls.Add(1) == 2 {
				<-releaseSender
			}
			return at
		}}
		_, err := svc.SendGroupTextMessage(context.Background(), groupMemberIdentity(), group.ID,
			clientUUIDv7(at, 829), "锁顺序竞争")
		sendDone <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("sender did not lock actor and group")
		}
		time.Sleep(10 * time.Millisecond)
	}
	blockTx, err := blocker.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer blockTx.Rollback(context.Background())
	if _, err := blockTx.Exec(context.Background(), `SELECT id FROM user_organizations
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantA, adminM); err != nil {
		t.Fatal(err)
	}
	blockDone := make(chan error, 1)
	go func() {
		if _, err := blockTx.Exec(context.Background(), `SELECT id FROM user_organizations
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantA, targetM2); err != nil {
			blockTx.Rollback(context.Background())
			blockDone <- err
			return
		}
		blockDone <- blockTx.Commit(context.Background())
	}()
	for {
		var waitType *string
		if err := conn.QueryRow(context.Background(), "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", blockerPID).Scan(&waitType); err != nil {
			t.Fatal(err)
		}
		if waitType != nil && *waitType == "Lock" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("membership end did not wait for sender lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(releaseSender)
	var sendErr, blockErr error
	select {
	case sendErr = <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sender stayed blocked after deadlock detection")
	}
	select {
	case blockErr = <-blockDone:
	case <-time.After(5 * time.Second):
		t.Fatal("membership update stayed blocked after deadlock detection")
	}
	if sendErr != nil || blockErr != nil || calls.Load() <= 5 {
		t.Fatalf("send did not retry lock cycle: send=%v block=%v calls=%d", sendErr, blockErr, calls.Load())
	}
	for table, want := range map[string]int{"messages": 1, "message_idempotency": 1, "outbox_events": 1} {
		var count int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", group.ID).Scan(&count); err != nil || count != want {
			t.Fatalf("%s after deadlock retry: %d %v", table, count, err)
		}
	}
	var audits int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
 WHERE action='message_send' AND resource_id=$1 AND outcome='allow'`, group.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("send audit after deadlock retry: %d %v", audits, err)
	}
}

func TestSendGroupTextMessageReplaysAfterLeaveButRejectsNewSend(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	clientID := clientUUIDv7(at, 806)
	first, err := svc.SendGroupTextMessage(context.Background(), groupMemberIdentity(), group.ID, clientID, "退群前")
	if err != nil {
		t.Fatal(err)
	}
	interval := groupIntervalFor(t, conn, group.ID, personA)
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, interval); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.SendGroupTextMessage(context.Background(), groupMemberIdentity(), group.ID, clientID, "退群前")
	if err != nil || replay.MessageID != first.MessageID || !replay.Duplicate {
		t.Fatalf("old ACK after leave: %+v %v", replay, err)
	}
	if _, err := svc.SendGroupTextMessage(context.Background(), groupMemberIdentity(), group.ID,
		clientUUIDv7(at, 807), "退群后"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("former member sent new message: %v", err)
	}
	second, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 808), "缺口")
	if err != nil || second.Seq != 2 {
		t.Fatalf("owner send during gap: %+v %v", second, err)
	}
	if _, err := svc.InviteGroupMember(context.Background(), publisher(), group.ID, inviteGroupRequest(targetM2)); err != nil {
		t.Fatal(err)
	}
	third, err := svc.SendGroupTextMessage(context.Background(), groupMemberIdentity(), group.ID,
		clientUUIDv7(at, 809), "重新入群")
	if err != nil || third.Seq != 3 {
		t.Fatalf("rejoined member send: %+v %v", third, err)
	}
	page, err := svc.PullGroupTextMessages(context.Background(), groupMemberIdentity(), group.ID, 0, 10)
	if err != nil || len(page.Messages) != 3 || page.Messages[0].Redacted ||
		!page.Messages[1].Redacted || page.Messages[2].Redacted {
		t.Fatalf("rejoin visibility: %+v %v", page, err)
	}
}

func TestSendGroupTextMessageBlocksWhenOtherMemberPairDenied(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "other-pair", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		SourceMembershipID: targetM2, TargetMembershipID: groupMemberC,
		EffectiveFrom: at.Add(-time.Hour), Reason: "pair blocked"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "pair blocked"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 810), "整群应阻断"); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("other pair policy ignored: %v", err)
	}
	var status string
	var seq, messages, decisions int
	if err := conn.QueryRow(context.Background(), "SELECT status,last_seq FROM conversations WHERE id=$1", group.ID).Scan(&status, &seq); err != nil || status != "policy_blocked" || seq != 0 {
		t.Fatalf("group not blocked atomically: %s %d %v", status, seq, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE conversation_id=$1", group.ID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("blocked message persisted: %d %v", messages, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='send_message' AND NOT allowed AND 'other-pair'=ANY(matched_rule_ids)").Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("pair denial audit: %d %v", decisions, err)
	}
}

func TestSendGroupTextMessageAuditsTimedPolicyDenial(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := writer.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "timed-stop", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		EffectiveFrom: at.Add(time.Second), Reason: "timed stop"}
	if _, err := writer.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "timed stop"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls >= 4 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 812), "临界发送"); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("timed hard deny missed: %v calls=%d", err, calls)
	}
	var denied int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM policy_decision_events
 WHERE action='send_message' AND NOT allowed AND 'timed-stop'=ANY(matched_rule_ids)`).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("timed denial lacked decision audit: %d %v", denied, err)
	}
}

func TestSendGroupTextMessageActorExpiresDuringWrite(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	writer := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := writer.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), adminM)
	calls := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls >= 4 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 813), "临界身份"); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired actor send: %v calls=%d", err, calls)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "active" {
		t.Fatalf("actor expiry changed group status: %s %v", status, err)
	}
}

func TestSendGroupTextMessageOutboxAndAuditFailureRollBack(t *testing.T) {
	for _, failure := range []string{"outbox", "audit"} {
		t.Run(failure, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
			if err != nil {
				t.Fatal(err)
			}
			if failure == "outbox" {
				run(t, conn, `CREATE FUNCTION fail_group_send_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END $$`)
				run(t, conn, "CREATE TRIGGER fail_group_send_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_group_send_outbox()")
			} else {
				run(t, conn, `CREATE FUNCTION fail_group_send_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
				run(t, conn, "CREATE TRIGGER fail_group_send_audit BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action='message_send') EXECUTE FUNCTION fail_group_send_audit()")
			}
			ack, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
				clientUUIDv7(at, 814), "失败回滚")
			if err == nil || ack.MessageID != "" || (failure == "audit" && !errors.Is(err, policystore.ErrAuditUnavailable)) {
				t.Fatalf("failed transaction acknowledged: %+v %v", ack, err)
			}
			var seq, messages, outboxes, reservations int
			if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", group.ID).Scan(&seq); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				query string
				out   *int
			}{
				{"SELECT count(*) FROM messages WHERE conversation_id=$1", &messages},
				{"SELECT count(*) FROM outbox_events WHERE conversation_id=$1", &outboxes},
				{"SELECT count(*) FROM message_rate_windows WHERE tenant_id=$1", &reservations},
			} {
				arg := group.ID
				if item.out == &reservations {
					arg = tenantA
				}
				if err := conn.QueryRow(context.Background(), item.query, arg).Scan(item.out); err != nil {
					t.Fatal(err)
				}
			}
			if seq != 0 || messages != 0 || outboxes != 0 || reservations != 0 {
				t.Fatalf("rollback residue: seq=%d messages=%d outbox=%d rate=%d", seq, messages, outboxes, reservations)
			}
		})
	}
}

func TestSendGroupTextMessageValidatesRateAndBlockedReplay(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }, MessageRatePerSecond: 1}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		clientID string
		body     string
		want     error
	}{
		{"bad", "x", policystore.ErrInvalidClientMessageID},
		{clientUUIDv7(at.Add(-7*24*time.Hour-time.Millisecond), 815), "x", policystore.ErrRetryExpired},
		{clientUUIDv7(at.Add(5*time.Minute+time.Millisecond), 816), "x", policystore.ErrInvalidClientMessageID},
		{clientUUIDv7(at, 817), "  ", policystore.ErrInvalidTextMessage},
		{clientUUIDv7(at, 818), string([]byte{0xff}), policystore.ErrInvalidTextMessage},
	} {
		if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
			input.clientID, input.body); !errors.Is(err, input.want) {
			t.Fatalf("invalid group payload %q: %v, want %v", input.body, err, input.want)
		}
	}
	clientID := clientUUIDv7(at, 819)
	first, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID, clientID, "首次")
	if err != nil || first.Seq != 1 {
		t.Fatalf("first message: %+v %v", first, err)
	}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 820), "限流"); !errors.Is(err, policystore.ErrMessageRateLimited) {
		t.Fatalf("rate limit missed: %v", err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	replay, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID, clientID, "首次")
	if err != nil || replay.MessageID != first.MessageID || !replay.Duplicate {
		t.Fatalf("blocked group replay: %+v %v", replay, err)
	}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 821), "阻断新消息"); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("blocked group accepted new message: %v", err)
	}
	run(t, conn, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientID, "首次"); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("frozen sender replayed ACK: %v", err)
	}
}

func TestSendGroupTextMessageConcurrentSequenceAndSameKey(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	open := func() *pgx.Conn {
		c, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	connections := []*pgx.Conn{open(), open()}
	runParallel := func(ids [2]string) ([2]policystore.MessageACK, [2]error) {
		var acks [2]policystore.MessageACK
		var errs [2]error
		var wg sync.WaitGroup
		for i, c := range connections {
			wg.Add(1)
			go func(i int, c *pgx.Conn) {
				defer wg.Done()
				acks[i], errs[i] = (policystore.Service{DB: c, Now: func() time.Time { return at }}).
					SendGroupTextMessage(context.Background(), publisher(), group.ID, ids[i], "并发")
			}(i, c)
		}
		wg.Wait()
		return acks, errs
	}
	acks, errs := runParallel([2]string{clientUUIDv7(at, 822), clientUUIDv7(at, 823)})
	if errs[0] != nil || errs[1] != nil || acks[0].Seq+acks[1].Seq != 3 || acks[0].Seq == acks[1].Seq {
		t.Fatalf("parallel group seq: %+v %+v, %v %v", acks[0], acks[1], errs[0], errs[1])
	}
	acks, errs = runParallel([2]string{clientUUIDv7(at, 824), clientUUIDv7(at, 824)})
	if errs[0] != nil || errs[1] != nil || acks[0].MessageID != acks[1].MessageID ||
		acks[0].Seq != 3 || acks[1].Seq != 3 || acks[0].Duplicate == acks[1].Duplicate {
		t.Fatalf("parallel same-key ACK: %+v %+v, %v %v", acks[0], acks[1], errs[0], errs[1])
	}
	var messages, events int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE conversation_id=$1", group.ID).Scan(&messages); err != nil || messages != 3 {
		t.Fatalf("parallel messages: %d %v", messages, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM outbox_events WHERE conversation_id=$1", group.ID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("parallel events: %d %v", events, err)
	}
}
