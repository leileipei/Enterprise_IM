package policystore_test

import (
	"context"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestRealtimeRecipientsValidateOutboxAndHistoricalUsers(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	ack, err := svc.SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 91), "通知")
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := conn.QueryRow(context.Background(), "SELECT id::text FROM outbox_events WHERE tenant_id=$1 AND message_id=$2", tenantA, ack.MessageID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	users, err := svc.ResolveMessageEvent(context.Background(), tenantA, eventID, directA, ack.MessageID, ack.Seq)
	if err != nil || len(users) != 2 || users[0] != adminA || users[1] != personA {
		t.Fatalf("historical recipients: %v %v", users, err)
	}
	for _, tc := range []struct {
		tenant, event, conversation, message string
		seq                                  int64
	}{
		{tenantB, eventID, directA, ack.MessageID, ack.Seq},
		{tenantA, "00000000-0000-4000-8000-000000000399", directA, ack.MessageID, ack.Seq},
		{tenantA, eventID, "00000000-0000-4000-8000-000000000398", ack.MessageID, ack.Seq},
		{tenantA, eventID, directA, "00000000-0000-4000-8000-000000000397", ack.Seq},
		{tenantA, eventID, directA, ack.MessageID, ack.Seq + 1},
	} {
		users, err := svc.ResolveMessageEvent(context.Background(), tc.tenant, tc.event, tc.conversation, tc.message, tc.seq)
		if err != nil || len(users) != 0 {
			t.Fatalf("forged event disclosed users: %v %v", users, err)
		}
	}
	run(t, conn, `UPDATE messages SET recipient_user_id=NULL,recipient_membership_id=NULL,
 sender_organization_id=NULL,recipient_organization_id=NULL WHERE id=$1`, ack.MessageID)
	users, err = svc.ResolveMessageEvent(context.Background(), tenantA, eventID, directA, ack.MessageID, ack.Seq)
	if err != nil || len(users) != 1 || users[0] != adminA {
		t.Fatalf("legacy recipient: %v %v", users, err)
	}
	conn.Close(context.Background())
	if _, err := svc.ResolveMessageEvent(context.Background(), tenantA, eventID, directA, ack.MessageID, ack.Seq); err == nil {
		t.Fatal("database failure was hidden")
	}
}

func TestRealtimeRecipientsResolveGroupIntervalsAtMessageSequence(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 901), "群通知一")
	if err != nil {
		t.Fatal(err)
	}
	memberInterval := groupIntervalFor(t, conn, group.ID, personA)
	if _, err := svc.LeaveGroup(context.Background(), groupMemberIdentity(), group.ID, memberInterval); err != nil {
		t.Fatal(err)
	}
	second, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID,
		clientUUIDv7(at, 902), "群通知二")
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(ack policystore.MessageACK) []string {
		var eventID string
		if err := conn.QueryRow(context.Background(), "SELECT id::text FROM outbox_events WHERE message_id=$1", ack.MessageID).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		users, err := svc.ResolveMessageEvent(context.Background(), tenantA, eventID, group.ID, ack.MessageID, ack.Seq)
		if err != nil {
			t.Fatal(err)
		}
		return users
	}
	firstUsers := resolve(first)
	secondUsers := resolve(second)
	if len(firstUsers) != 3 || !containsUser(firstUsers, adminA) || !containsUser(firstUsers, personA) ||
		!containsUser(firstUsers, groupUserC) || len(secondUsers) != 2 ||
		!containsUser(secondUsers, adminA) || !containsUser(secondUsers, groupUserC) ||
		containsUser(secondUsers, personA) {
		t.Fatalf("group historical recipients: first=%v second=%v", firstUsers, secondUsers)
	}
}

func TestRealtimeRecipientsRejectGroupMessageWithWrongSourceMembership(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	insertGroupHistoryMessage(t, conn, group.ID, 1, personA, targetM, "错误任职")
	var messageID string
	if err := conn.QueryRow(context.Background(), "SELECT id::text FROM messages WHERE conversation_id=$1 AND seq=1", group.ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	run(t, conn, `INSERT INTO outbox_events (tenant_id,conversation_id,seq,message_id,event_type)
 VALUES ($1,$2,1,$3,'message_created')`, tenantA, group.ID, messageID)
	var eventID string
	if err := conn.QueryRow(context.Background(), "SELECT id::text FROM outbox_events WHERE message_id=$1", messageID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	users, err := svc.ResolveMessageEvent(context.Background(), tenantA, eventID, group.ID, messageID, 1)
	if err != nil || len(users) != 0 {
		t.Fatalf("forged group sender was signaled: %v %v", users, err)
	}
}

func containsUser(users []string, want string) bool {
	for _, user := range users {
		if user == want {
			return true
		}
	}
	return false
}
