package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"reflect"
	"testing"
	"time"
)

func TestReceiptDeniedOperationsAreAudited(t *testing.T) {
	for _, kind := range []string{"missing_delivery", "sender_query", "scan_gap", "expired_proof", "group_names", "late_device", "sync_unavailable", "unread_unavailable"} {
		t.Run(kind, func(t *testing.T) {
			c, s, d := receiptDirectFixture(t)
			ctx := context.Background()
			action := "message_read_receipt"
			var err error
			switch kind {
			case "missing_delivery":
				_, err = s.ConfirmRead(ctx, groupMemberIdentity(), d, directA, []int64{1})
			case "sender_query":
				action = "message_receipt_query"
				_, err = s.QueryReceipts(ctx, groupMemberIdentity(), d, directA, []int64{1})
			case "scan_gap":
				action = "message_delivered"
				if _, e := s.SendTextMessage(ctx, publisher(), directA, clientUUIDv7(at, 9), "second"); e != nil {
					t.Fatal(e)
				}
				p, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), d, directA, 1, 1)
				if e != nil {
					t.Fatal(e)
				}
				_, err = s.ConfirmDelivered(ctx, groupMemberIdentity(), d, directA, p.SyncProof)
			case "expired_proof":
				action = "message_delivered"
				p, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), d, directA, 0, 1)
				if e != nil {
					t.Fatal(e)
				}
				s.Now = func() time.Time { return at.Add(301 * time.Second) }
				_, err = s.ConfirmDelivered(ctx, groupMemberIdentity(), d, directA, p.SyncProof)
			case "group_names":
				action = "message_receipt_users"
				g, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
				if e != nil {
					t.Fatal(e)
				}
				insertGroupHistoryMessage(t, c, g.ID, 1, adminA, adminM, "owner")
				_, err = s.ListReceiptUsers(ctx, groupMemberIdentity(), d, g.ID, 1, "", 20)
			case "sync_unavailable":
				action = "device_sync"
				_, err = s.SyncDeviceMessages(ctx, groupMemberIdentity(), d, tenantB, 0, 100)
			case "unread_unavailable":
				action = "message_unread_query"
				_, err = s.GetUnread(ctx, groupMemberIdentity(), d, tenantB)
			case "late_device":
				receiptDeliver(t, s, d, directA)
				clock := at
				s.Now = func() time.Time { return clock }
				s.DB = afterReceiptStateDB{Beginner: c, after: func() { clock = at.Add(30 * 24 * time.Hour) }}
				_, err = s.ConfirmRead(ctx, groupMemberIdentity(), d, directA, []int64{1})
			}
			if err == nil {
				t.Fatal("denied operation succeeded")
			}
			var n int
			if e := c.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action=$1 AND outcome='deny' AND actor_user_id=$2 AND acting_membership_id=$3", action, personA, targetM2).Scan(&n); e != nil || n != 1 {
				t.Fatalf("denial missing %s: count=%d err=%v", action, n, e)
			}
			if e := c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 0 {
				t.Fatal("denial committed read", n, e)
			}
			if kind == "late_device" {
				if e := c.QueryRow(ctx, "SELECT count(*) FROM receipt_outbox_events").Scan(&n); e != nil || n != 1 {
					t.Fatal("late denial committed tentative outbox", n, e)
				}
				if e := c.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='message_read_receipt' AND outcome='allow'").Scan(&n); e != nil || n != 0 {
					t.Fatal("late denial committed allow audit", n, e)
				}
			}
		})
	}
}
func TestReceiptDeniedAuditFailureIsUnavailable(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	run(t, c, `CREATE FUNCTION fail_denied_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.outcome='deny' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_denied_receipt BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_denied_receipt()`)
	if _, e := s.ConfirmRead(context.Background(), groupMemberIdentity(), d, directA, []int64{1}); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal("denial without durable audit", e)
	}
}
func TestSyncZeroCacheHasBoundedOnlineGrant(t *testing.T) {
	c, s, d := receiptDirectFixture(t)
	ctx := context.Background()
	run(t, c, "INSERT INTO tenant_client_policies(tenant_id,offline_cache_seconds) VALUES($1,0)", tenantA)
	p, e := s.SyncDeviceMessages(ctx, groupMemberIdentity(), d, directA, 0, 1)
	if e != nil {
		t.Fatal(e)
	}
	lease := p.ContentLeases[1]
	if !lease.CacheUntil.Equal(p.ServerTime) {
		t.Fatal("zero cache granted offline use")
	}
	f := reflect.ValueOf(lease).FieldByName("OnlineUntil")
	if !f.IsValid() {
		t.Fatal("online-only display grant missing")
	}
	online := f.Interface().(time.Time)
	if !online.Equal(p.ServerTime.Add(5 * time.Second)) {
		t.Fatal("online display budget", online)
	}
	run(t, c, "UPDATE user_devices SET expires_at=$1 WHERE id=$2", at.Add(2*time.Second), d.ID)
	p, e = s.SyncDeviceMessages(ctx, groupMemberIdentity(), d, directA, 0, 1)
	if e != nil {
		t.Fatal(e)
	}
	online = reflect.ValueOf(p.ContentLeases[1]).FieldByName("OnlineUntil").Interface().(time.Time)
	if !online.Equal(at.Add(2 * time.Second)) {
		t.Fatal("device bound enlarged", online)
	}
}
