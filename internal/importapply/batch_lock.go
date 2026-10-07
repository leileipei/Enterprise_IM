package importapply

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"regexp"
	"strings"
	"time"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func batchLockKey(tenantID, requestID string) int64 {
	h := sha256.New()
	h.Write([]byte("enterprise_im:controlled_append_v1"))
	for _, s := range []string{tenantID, requestID} {
		b, _ := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
		h.Write(b)
	}
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]))
}

// One cleanup deadline covers both rollback and unlock. Unverified connections are discarded.
func cleanup(conn *pgxpool.Conn, tx pgx.Tx, locked bool, key int64, b *sqlBudget) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	clean := true
	if tx != nil {
		if b.charge(ctx, controlSQL) != nil {
			clean = false
		} else if e := tx.Rollback(ctx); e != nil && e != pgx.ErrTxClosed {
			clean = false
		}
	}
	if locked {
		var released bool
		if b.charge(ctx, controlSQL) != nil {
			clean = false
		} else if e := conn.QueryRow(ctx, "SELECT pg_catalog.pg_advisory_unlock($1)", key).Scan(&released); e != nil || !released {
			clean = false
		}
	}
	if !clean || conn.Conn().IsClosed() {
		raw := conn.Hijack()
		_ = raw.Close(ctx)
		_ = raw.PgConn().Conn().Close()
		return
	}
	conn.Release()
}
func directContext(ctx context.Context, b *sqlBudget, kind sqlKind) (context.Context, context.CancelFunc, error) {
	if e := b.charge(ctx, kind); e != nil {
		return nil, nil, e
	}
	end := time.Now().Add(5 * time.Second)
	if b.Deadline.Before(end) {
		end = b.Deadline
	}
	c, cancel := context.WithDeadline(ctx, end)
	return c, cancel, nil
}

func begin(ctx context.Context, conn *pgxpool.Conn, iso pgx.TxIsoLevel, b *sqlBudget) (pgx.Tx, error) {
	sub, cancel, e := directContext(ctx, b, controlSQL)
	if e != nil {
		return nil, e
	}
	defer cancel()
	return conn.BeginTx(sub, pgx.TxOptions{IsoLevel: iso})
}
