package policystore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func clientUUIDv7(at time.Time, tail int) string {
	milliseconds := uint64(at.UnixMilli())
	return fmt.Sprintf("%08x-%04x-7000-8000-%012x", milliseconds>>16, milliseconds&0xffff, tail)
}

func TestSendTextMessageCommitsACKAndOutboxOnce(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	clientID := clientUUIDv7(at, 1)
	first, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientID, "你好")
	if err != nil || first.MessageID == "" || first.ConversationID != directA || first.Seq != 1 || !first.ServerTime.Equal(at) {
		t.Fatalf("first durable ACK: %+v %v", first, err)
	}
	replay, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientID, "你好")
	if err != nil || replay.MessageID != first.MessageID || replay.ConversationID != first.ConversationID ||
		replay.Seq != first.Seq || !replay.ServerTime.Equal(first.ServerTime) {
		t.Fatalf("retry changed ACK: %+v %+v %v", first, replay, err)
	}
	for table, want := range map[string]int{"messages": 1, "message_idempotency": 1, "outbox_events": 1} {
		var count int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE tenant_id=$1", tenantA).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count: %d %v", table, count, err)
		}
	}
	var seq int64
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", directA).Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("last_seq: %d %v", seq, err)
	}
	var recipientUser, recipientMember, senderOrg, recipientOrg string
	if err := conn.QueryRow(context.Background(), "SELECT recipient_user_id::text,recipient_membership_id::text,sender_organization_id::text,recipient_organization_id::text FROM messages WHERE id=$1", first.MessageID).Scan(&recipientUser, &recipientMember, &senderOrg, &recipientOrg); err != nil || recipientUser != personA || recipientMember != targetM2 || senderOrg != orgA || recipientOrg != orgA {
		t.Fatalf("recipient and organizations at send: %s %s %s %s %v", recipientUser, recipientMember, senderOrg, recipientOrg, err)
	}
	var decisions, audits, sentCount int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='send_message'").Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("send decisions: %d %v", decisions, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("send request audits: %d %v", audits, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT sent_count FROM message_rate_windows WHERE tenant_id=$1 AND sender_user_id=$2", tenantA, adminA).Scan(&sentCount); err != nil || sentCount != 1 {
		t.Fatalf("retry consumed rate limit: %d %v", sentCount, err)
	}
}

func TestSendTextMessageRejectsDifferentContentForSameKey(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	clientID := clientUUIDv7(at, 2)
	first, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientID, "内容 A")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientID, "内容 B")
	if !errors.Is(err, policystore.ErrIdempotencyConflict) || second.MessageID != "" {
		t.Fatalf("different content reused key: %+v %v", second, err)
	}
	var seq int64
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", directA).Scan(&seq); err != nil || seq != first.Seq {
		t.Fatalf("conflict consumed sequence: %d %v", seq, err)
	}
}

func TestSendTextMessageCanonicalizesConversationIDInACK(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	conversationID := "aaaaaaaa-0000-4000-8000-000000000401"
	run(t, conn, "UPDATE conversations SET id=$1 WHERE id=$2", conversationID, directA)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	clientID := clientUUIDv7(at, 21)
	first, err := svc.SendTextMessage(context.Background(), publisher(), strings.ToUpper(conversationID), clientID, "same")
	if err != nil || first.ConversationID != conversationID {
		t.Fatalf("noncanonical first ACK: %+v %v", first, err)
	}
	replay, err := svc.SendTextMessage(context.Background(), publisher(), conversationID, clientID, "same")
	if err != nil || replay.MessageID != first.MessageID || replay.ConversationID != first.ConversationID || replay.Seq != first.Seq || !replay.ServerTime.Equal(first.ServerTime) {
		t.Fatalf("retry changed canonical ACK: %+v %+v %v", first, replay, err)
	}
}

func TestSendTextMessageValidatesInputAndIdentity(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, tc := range []struct {
		id   string
		body string
		want error
	}{
		{"bad", "hello", policystore.ErrInvalidClientMessageID},
		{clientUUIDv7(at.Add(-7*24*time.Hour-time.Millisecond), 3), "hello", policystore.ErrRetryExpired},
		{clientUUIDv7(at.Add(5*time.Minute+time.Millisecond), 4), "hello", policystore.ErrInvalidClientMessageID},
		{clientUUIDv7(at, 5), "   ", policystore.ErrInvalidTextMessage},
		{clientUUIDv7(at, 6), string([]byte{0xff}), policystore.ErrInvalidTextMessage},
		{clientUUIDv7(at, 7), string(make([]byte, 16*1024+1)), policystore.ErrInvalidTextMessage},
	} {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, tc.id, tc.body); !errors.Is(err, tc.want) {
			t.Fatalf("input %s: %v, want %v", tc.id, err, tc.want)
		}
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err := svc.SendTextMessage(context.Background(), foreign, directA, clientUUIDv7(at, 8), "hello"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("cross-tenant conversation: %v", err)
	}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), "00000000-0000-4000-8000-000000000499", clientUUIDv7(at, 8), "hello"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
		t.Fatalf("missing conversation: %v", err)
	}
	var messages int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("invalid request wrote messages: %d %v", messages, err)
	}
}

func TestSendTextMessageInvalidActorCannotProbeConversation(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	for _, conversationID := range []string{directA, "00000000-0000-4000-8000-000000000499"} {
		if _, err := svc.SendTextMessage(context.Background(), publisher(), conversationID, clientUUIDv7(at, 22), "hello"); !errors.Is(err, policystore.ErrForbidden) {
			t.Fatalf("invalid actor could probe %s: %v", conversationID, err)
		}
	}
}

func TestSendTextMessageRateLimitAndReplay(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }, MessageRatePerSecond: 1}
	firstID := clientUUIDv7(at, 10)
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, firstID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, firstID, "first"); err != nil {
		t.Fatalf("replay hit rate limit: %v", err)
	}
	if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 11), "second"); !errors.Is(err, policystore.ErrMessageRateLimited) {
		t.Fatalf("second new message: %v", err)
	}
	var seq int
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", directA).Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("rate rejection consumed seq: %d %v", seq, err)
	}
}

func TestSendTextMessageRechecksIdentityPolicyAndConversation(t *testing.T) {
	t.Run("frozen replay", func(t *testing.T) {
		conn := db(t)
		seedDirectConversation(t, conn)
		svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
		id := clientUUIDv7(at, 12)
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, id, "first"); err != nil {
			t.Fatal(err)
		}
		run(t, conn, "UPDATE users SET status='frozen' WHERE tenant_id=$1 AND id=$2", tenantA, adminA)
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, id, "first"); !errors.Is(err, policystore.ErrForbidden) {
			t.Fatalf("frozen actor got replay: %v", err)
		}
	})
	t.Run("new send after hard deny", func(t *testing.T) {
		conn := db(t)
		seedDirectConversation(t, conn)
		grantPublisher(t, conn)
		svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
		rule := policy.Rule{ID: "stop-messages", TenantID: tenantA, Effect: policy.EffectHardDeny,
			Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
			Reason: "暂停通信", EffectiveFrom: at.Add(-time.Hour)}
		if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "暂停消息"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 13), "blocked"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
			t.Fatalf("hard deny: %v", err)
		}
		var messages int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&messages); err != nil || messages != 0 {
			t.Fatalf("blocked message persisted: %d %v", messages, err)
		}
	})
	t.Run("ended conversation", func(t *testing.T) {
		conn := db(t)
		seedDirectConversation(t, conn)
		run(t, conn, "UPDATE conversations SET status='ended' WHERE id=$1", directA)
		svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
		if _, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 14), "blocked"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
			t.Fatalf("ended conversation: %v", err)
		}
	})
}

func TestSendTextMessageOutboxFailureRollsBackEverything(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	run(t, conn, `CREATE FUNCTION fail_message_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END $$`)
	run(t, conn, "CREATE TRIGGER fail_message_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_message_outbox()")
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if ack, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 15), "failed"); err == nil || ack.MessageID != "" {
		t.Fatalf("outbox failure acknowledged: %+v %v", ack, err)
	}
	run(t, conn, "DROP TRIGGER fail_message_outbox ON outbox_events")
	ack, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 15), "retry")
	if err != nil || ack.Seq != 1 {
		t.Fatalf("rollback left seq or rate residue: %+v %v", ack, err)
	}
	var messages, outboxes, reservations int
	for _, item := range []struct {
		query string
		count *int
	}{
		{"SELECT count(*) FROM messages", &messages},
		{"SELECT count(*) FROM outbox_events", &outboxes},
		{"SELECT sent_count FROM message_rate_windows WHERE tenant_id=$1 AND sender_user_id=$2", &reservations},
	} {
		var e error
		if item.count == &reservations {
			e = conn.QueryRow(context.Background(), item.query, tenantA, adminA).Scan(item.count)
		} else {
			e = conn.QueryRow(context.Background(), item.query).Scan(item.count)
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	if messages != 1 || outboxes != 1 || reservations != 1 {
		t.Fatalf("rollback residue: messages=%d outbox=%d rate=%d", messages, outboxes, reservations)
	}
}

func TestSendTextMessageAuditFailureRollsBackEverything(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	run(t, conn, `CREATE FUNCTION fail_message_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	run(t, conn, "CREATE TRIGGER fail_message_audit BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'message_send') EXECUTE FUNCTION fail_message_audit()")
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if ack, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 19), "failed"); !errors.Is(err, policystore.ErrAuditUnavailable) || ack.MessageID != "" {
		t.Fatalf("audit failure acknowledged: %+v %v", ack, err)
	}
	var seq, messages, outboxes, reservations int
	for _, item := range []struct {
		query string
		out   *int
	}{
		{"SELECT last_seq FROM conversations WHERE tenant_id=$1", &seq},
		{"SELECT count(*) FROM messages WHERE tenant_id=$1", &messages},
		{"SELECT count(*) FROM outbox_events WHERE tenant_id=$1", &outboxes},
		{"SELECT count(*) FROM message_rate_windows WHERE tenant_id=$1", &reservations},
	} {
		if err := conn.QueryRow(context.Background(), item.query, tenantA).Scan(item.out); err != nil {
			t.Fatal(err)
		}
	}
	if seq != 0 || messages != 0 || outboxes != 0 || reservations != 0 {
		t.Fatalf("audit rollback residue: seq=%d messages=%d outbox=%d rate=%d", seq, messages, outboxes, reservations)
	}
}

func TestSendTextMessageRejectsReselectedMembershipDuringWrite(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	other, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close(context.Background()) })
	if _, err := other.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		calls++
		if calls == 2 {
			if _, err := other.Exec(context.Background(), "UPDATE conversations SET direct_high_membership_id=$1 WHERE id=$2", targetM, directA); err != nil {
				t.Fatal(err)
			}
		}
		return at
	}}
	if ack, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 20), "old context"); !errors.Is(err, policystore.ErrConversationContextChanged) || ack.MessageID != "" {
		t.Fatalf("membership reselection accepted: %+v %v", ack, err)
	}
	var seq int
	if err := conn.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", directA).Scan(&seq); err != nil || seq != 0 {
		t.Fatalf("reselection consumed seq: %d %v", seq, err)
	}
}

func TestSendTextMessageConcurrentSequenceAndIdempotency(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
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
	first, second := open(), open()
	ids := []string{clientUUIDv7(at, 16), clientUUIDv7(at, 17)}
	results := make([]policystore.MessageACK, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, dbconn := range []*pgx.Conn{first, second} {
		wg.Add(1)
		go func(i int, c *pgx.Conn) {
			defer wg.Done()
			results[i], errs[i] = (policystore.Service{DB: c, Now: func() time.Time { return at }}).SendTextMessage(context.Background(), publisher(), directA, ids[i], "parallel")
		}(i, dbconn)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].Seq+results[1].Seq != 3 || results[0].Seq == results[1].Seq {
		t.Fatalf("parallel seq: %+v %+v, %v %v", results[0], results[1], errs[0], errs[1])
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&count); err != nil || count != 2 {
		t.Fatalf("parallel messages: %d %v", count, err)
	}
	// Two requests with the same key must converge to one durable ACK.
	for i := range errs {
		errs[i] = nil
		results[i] = policystore.MessageACK{}
	}
	for i, dbconn := range []*pgx.Conn{first, second} {
		wg.Add(1)
		go func(i int, c *pgx.Conn) {
			defer wg.Done()
			results[i], errs[i] = (policystore.Service{DB: c, Now: func() time.Time { return at }}).SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 18), "same")
		}(i, dbconn)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].MessageID != results[1].MessageID || results[0].Seq != 3 || results[1].Seq != 3 {
		t.Fatalf("same-key parallel ACK: %+v %+v, %v %v", results[0], results[1], errs[0], errs[1])
	}
}
