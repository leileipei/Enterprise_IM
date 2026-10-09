package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestReceiptConcurrentTwoDevicesNoCountOrOutboxDuplication(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receiptDeliver(t, s, device, directA)
	g, e := s.RegisterDevice(ctx, groupMemberIdentity(), deviceRegistration(2))
	if e != nil {
		t.Fatal(e)
	}
	otherDevice := policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}
	receiptDeliver(t, s, otherDevice, directA)
	peer1, peer2 := filePeer(t, c), filePeer(t, c)
	done := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, e := deviceService(peer1).ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1})
		done <- e
	}()
	go func() {
		<-start
		_, e := deviceService(peer2).ConfirmRead(ctx, groupMemberIdentity(), otherDevice, directA, []int64{1})
		done <- e
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if e = <-done; e != nil {
			t.Fatal(e)
		}
	}
	for table, want := range map[string]int{"message_user_receipts": 1, "message_device_receipts": 2, "receipt_outbox_events": 2, "receipt_outbox_targets": 4} {
		var n int
		if e = c.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != want {
			t.Fatal("duplicate facts/events", table, n, e)
		}
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL").Scan(&n); e != nil || n != 1 {
		t.Fatal("user read missing")
	}
}
func TestReadRevokedDeviceRaceAndHistoricalStateRetained(t *testing.T) {
	c, s, device := receiptDirectFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receiptDeliver(t, s, device, directA)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "UPDATE user_devices SET revoked_at=$2 WHERE id=$1", device.ID, at); e != nil {
		t.Fatal(e)
	}
	peer := filePeer(t, c)
	observer := filePeer(t, c)
	done := make(chan error, 1)
	go func() {
		_, e := deviceService(peer).ConfirmRead(ctx, groupMemberIdentity(), device, directA, []int64{1})
		done <- e
	}()
	waitFileSendLock(t, observer, peer.PgConn().PID())
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; !errors.Is(e, policystore.ErrInvalidDevice) {
		t.Fatal("revocation bypassed", e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM message_user_receipts WHERE read_at IS NULL").Scan(&n); e != nil || n != 1 {
		t.Fatal("delivery lost or false read")
	}
}
