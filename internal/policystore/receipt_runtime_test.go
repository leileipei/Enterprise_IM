package policystore_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestReceiptRuntimeSchemaAndSharedWriteRate(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	if e := s.CheckReceiptRuntime(ctx); e != nil {
		t.Fatal("complete runtime", e)
	}
	s.ReceiptWriteRatePerSecond = 2
	if e := s.CheckReceiptWriteRate(ctx, deviceOwner()); e != nil {
		t.Fatal(e)
	}
	peer := filePeer(t, c)
	other := deviceService(peer)
	other.ReceiptWriteRatePerSecond = 2
	if e := other.CheckReceiptWriteRate(ctx, deviceOwner()); e != nil {
		t.Fatal(e)
	}
	if e := s.CheckReceiptWriteRate(ctx, deviceOwner()); !errors.Is(e, policystore.ErrReceiptRateLimited) {
		t.Fatal("node-local rate bypass", e)
	}
	s.Now = func() time.Time { return at.Add(time.Second) }
	if e := s.CheckReceiptWriteRate(ctx, deviceOwner()); e != nil {
		t.Fatal("window rollover", e)
	}
	run(t, c, "ALTER TABLE user_devices DROP CONSTRAINT user_devices_tenant_id_user_id_registration_id_key")
	if e := s.CheckReceiptRuntime(ctx); e == nil {
		t.Fatal("missing registration uniqueness accepted")
	}
	run(t, c, "ALTER TABLE user_devices ADD CONSTRAINT user_devices_tenant_id_user_id_registration_id_key UNIQUE(tenant_id,user_id,registration_id)")
	run(t, c, "ALTER TABLE message_user_receipts DISABLE TRIGGER message_user_receipts_protected")
	if e := s.CheckReceiptRuntime(ctx); e == nil {
		t.Fatal("disabled fact protection accepted")
	}
	run(t, c, "ALTER TABLE message_user_receipts ENABLE TRIGGER message_user_receipts_protected")
	run(t, c, "DROP TABLE receipt_write_windows")
	if e := s.CheckReceiptRuntime(ctx); e == nil {
		t.Fatal("partial migrations accepted")
	}
}

func TestReceiptRuntimeRateMigrationRollback(t *testing.T) {
	c := db(t)
	ctx := context.Background()
	for _, direction := range []string{"down", "up"} {
		b, e := os.ReadFile("../../db/migrations/000025_receipt_write_rate." + direction + ".sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	seed(t, c)
	s := deviceService(c)
	if e := s.CheckReceiptWriteRate(ctx, deviceOwner()); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile("../../db/migrations/000025_receipt_write_rate.down.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.PgConn().Exec(ctx, string(b)).ReadAll(); e == nil {
		t.Fatal("rate window was silently dropped")
	}
	run(t, c, "ROLLBACK")
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM receipt_write_windows").Scan(&n); e != nil || n != 1 {
		t.Fatal("rate window lost", e)
	}
}
