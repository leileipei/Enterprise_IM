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
