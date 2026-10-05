package policystore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func digestConversation(t *testing.T, conn *pgx.Conn, kind string) (policystore.Service, string) {
	t.Helper()
	seedDirectConversation(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	cid := directA
	if kind == "group" {
		g, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
		if err != nil {
			t.Fatal(err)
		}
		cid = g.ID
	}
	return svc, cid
}
func digestSend(svc policystore.Service, kind string, ctx context.Context, id access.TrustedIdentity, cid, client, body string) (policystore.MessageACK, error) {
	if kind == "group" {
		return svc.SendGroupTextMessage(ctx, id, cid, client, body)
	}
	return svc.SendTextMessage(ctx, id, cid, client, body)
}
func clearForDigest(t *testing.T, conn *pgx.Conn, mid string) {
	t.Helper()
	run(t, conn, `UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1`, mid, at.Add(24*time.Hour))
}
func assertDigestNoNewWrites(t *testing.T, conn *pgx.Conn, cid string, want int) {
	t.Helper()
	for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
		var count int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", cid).Scan(&count); err != nil || count != want {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
	var seq int
	var rate int
	if err := conn.QueryRow(context.Background(), `SELECT last_seq FROM conversations WHERE id=$1`, cid).Scan(&seq); err != nil || seq != want {
		t.Fatalf("seq: %d %v", seq, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT sent_count FROM message_rate_windows WHERE tenant_id=$1 AND sender_user_id=$2`, tenantA, adminA).Scan(&rate); err != nil || rate != want {
		t.Fatalf("rate: %d %v", rate, err)
	}
}
func TestDigestRetiredMessagesRejectReplay(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			conn := db(t)
			svc, cid := digestConversation(t, conn, kind)
			ctx := context.Background()
			client := clientUUIDv7(at, 1200)
			first, err := digestSend(svc, kind, ctx, publisher(), cid, client, "secret")
			if err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 2; n++ {
				if _, err := digestSend(svc, kind, ctx, publisher(), cid, clientUUIDv7(at, 1200+n), "visible"); err != nil {
					t.Fatal(err)
				}
			}
			clearForDigest(t, conn, first.MessageID)
			ack, err := digestSend(svc, kind, ctx, publisher(), cid, client, "secret")
			if err != nil || ack.MessageID != first.MessageID || !ack.Duplicate {
				t.Fatalf("pre-retirement: %+v %v", ack, err)
			}
			if _, err := digestSend(svc, kind, ctx, publisher(), cid, client, "changed"); !errors.Is(err, policystore.ErrIdempotencyConflict) {
				t.Fatalf("conflict: %v", err)
			}
			retireDigestPair(t, conn, first.MessageID, at.Add(31*24*time.Hour))
			for _, now := range []time.Time{at.Add(31 * 24 * time.Hour), at} {
				svc.Now = func() time.Time { return now }
				for _, body := range []string{"secret", "changed"} {
					ack, err := digestSend(svc, kind, ctx, publisher(), cid, client, body)
					if !errors.Is(err, policystore.ErrRetryExpired) || ack.MessageID != "" {
						t.Fatalf("retired: %+v %v", ack, err)
					}
				}
			}
			assertDigestNoNewWrites(t, conn, cid, 3)
			pull := svc.PullTextMessages
			if kind == "group" {
				pull = svc.PullGroupTextMessages
			}
			page, err := pull(ctx, publisher(), cid, 0, 2)
			if err != nil || len(page.Messages) != 2 || !page.HasMore || page.NextAfterSeq != 2 {
				t.Fatalf("page: %+v %v", page, err)
			}
			assertBodyPlaceholder(t, page.Messages[0], 1)
		})
	}
}

func TestDigestRetiredReplayAuthorizationAndCorruption(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			conn := db(t)
			svc, cid := digestConversation(t, conn, kind)
			client := clientUUIDv7(at, 1300)
			ack, err := digestSend(svc, kind, context.Background(), publisher(), cid, client, "secret")
			if err != nil {
				t.Fatal(err)
			}
			clearForDigest(t, conn, ack.MessageID)
			retireDigestPair(t, conn, ack.MessageID, at.Add(31*24*time.Hour))
			bad := publisher()
			bad.TenantID = tenantB
			bad.UserID = personB
			bad.ActingMembershipID = otherM
			if _, err := digestSend(svc, kind, context.Background(), bad, cid, client, "secret"); errors.Is(err, policystore.ErrRetryExpired) || err == nil {
				t.Fatalf("cross tenant learned retired state: %v", err)
			}
			bad = publisher()
			bad.ActingMembershipID = targetM
			if _, err := digestSend(svc, kind, context.Background(), bad, cid, client, "secret"); !errors.Is(err, policystore.ErrForbidden) {
				t.Fatalf("invalid identity: %v", err)
			}

			outsiderUser := "00000000-0000-4000-8000-000000009901"
			outsiderMember := "00000000-0000-4000-8000-000000009902"
			run(t, conn, "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'outsider','outsider')", outsiderUser, tenantA)
			run(t, conn, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", outsiderMember, tenantA, outsiderUser, orgA)
			bad = access.TrustedIdentity{TenantID: tenantA, UserID: outsiderUser, ActingMembershipID: outsiderMember}
			if _, err := digestSend(svc, kind, context.Background(), bad, cid, client, "secret"); !errors.Is(err, policystore.ErrMessageNotAvailable) {
				t.Fatalf("outsider learned retired state: %v", err)
			}
			assertDigestNoNewWrites(t, conn, cid, 1)
		})
	}
	for _, damage := range []string{"missing digest", "different digest", "missing row"} {
		t.Run(damage, func(t *testing.T) {
			conn := db(t)
			svc, cid := digestConversation(t, conn, "direct")
			client := clientUUIDv7(at, 1400)
			_, err := digestSend(svc, "direct", context.Background(), publisher(), cid, client, "secret")
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing digest":
				run(t, conn, "ALTER TABLE message_idempotency DROP CONSTRAINT idempotency_digest_state")
				run(t, conn, "UPDATE message_idempotency SET content_digest=NULL")
			case "different digest":
				run(t, conn, "UPDATE message_idempotency SET content_digest=decode(repeat('aa',32),'hex')")
			case "missing row":
				run(t, conn, "DELETE FROM message_idempotency")
			}
			ack, err := digestSend(svc, "direct", context.Background(), publisher(), cid, client, "secret")
			if err == nil || errors.Is(err, policystore.ErrIdempotencyConflict) || errors.Is(err, policystore.ErrRetryExpired) || ack.MessageID != "" {
				t.Fatalf("corrupt state not unavailable: %+v %v", ack, err)
			}
			var count int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
				t.Fatalf("corruption recreated message: %d %v", count, err)
			}
		})
	}
}

type digestGateDB struct {
	access.Beginner
	match   func(string) bool
	reached chan<- struct{}
	release <-chan struct{}
}

func (db digestGateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.Beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return digestGateTx{Tx: tx, db: db, ctx: ctx}, nil
}

type digestGateTx struct {
	pgx.Tx
	db  digestGateDB
	ctx context.Context
}

func (tx digestGateTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return digestGateRow{Row: tx.Tx.QueryRow(ctx, sql, args...), tx: tx, sql: sql}
}

type digestGateRow struct {
	pgx.Row
	tx  digestGateTx
	sql string
}

func (row digestGateRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	if row.tx.db.match(row.sql) {
		select {
		case row.tx.db.reached <- struct{}{}:
		case <-row.tx.ctx.Done():
			return row.tx.ctx.Err()
		}
		select {
		case <-row.tx.db.release:
		case <-row.tx.ctx.Done():
			return row.tx.ctx.Err()
		}
	}
	return nil
}
func TestDigestRetiredSendSnapshots(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		for _, point := range []string{"before lookup", "after lookup"} {
			t.Run(kind+"/"+point, func(t *testing.T) {
				conn := db(t)
				svc, cid := digestConversation(t, conn, kind)
				client := clientUUIDv7(at, 1500)
				first, err := digestSend(svc, kind, context.Background(), publisher(), cid, client, "secret")
				if err != nil {
					t.Fatal(err)
				}
				clearForDigest(t, conn, first.MessageID)
				second := secondDB(t, conn)
				run(t, conn, "SET default_transaction_isolation='repeatable read'")
				reached, release := make(chan struct{}, 1), make(chan struct{})
				firstQuery := true
				svc.DB = digestGateDB{Beginner: conn, reached: reached, release: release, match: func(sql string) bool {
					if point == "after lookup" {
						return strings.Contains(sql, "JOIN message_idempotency") || strings.Contains(sql, "FROM message_idempotency i JOIN messages")
					}
					if firstQuery {
						firstQuery = false
						return true
					}
					return false
				}}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				type outcome struct {
					ack policystore.MessageACK
					err error
				}
				done := make(chan outcome, 1)
				go func() { a, e := digestSend(svc, kind, ctx, publisher(), cid, client, "secret"); done <- outcome{a, e} }()
				select {
				case <-reached:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if point == "after lookup" && kind == "group" {
					retirement := make(chan error, 1)
					go func() {
						tx, err := second.Begin(ctx)
						if err != nil {
							retirement <- err
							return
						}
						defer tx.Rollback(context.Background())
						if _, err = tx.Exec(ctx, "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", cid); err == nil {
							_, err = tx.Exec(ctx, "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", first.MessageID, at.Add(31*24*time.Hour))
						}
						if err == nil {
							_, err = tx.Exec(ctx, "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", first.MessageID, at.Add(31*24*time.Hour))
						}
						if err == nil {
							err = tx.Commit(ctx)
						}
						retirement <- err
					}()
					for {
						var wait *string
						if err := conn.QueryRow(ctx, "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", second.PgConn().PID()).Scan(&wait); err != nil {
							t.Fatal(err)
						}
						if wait != nil && *wait == "Lock" {
							break
						}
						select {
						case err := <-retirement:
							t.Fatalf("retirement did not wait: %v", err)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(time.Millisecond):
						}
					}
					close(release)
					got := <-done
					if got.err != nil || got.ack.MessageID != first.MessageID {
						t.Fatalf("prior group lookup: %+v", got)
					}
					if err := <-retirement; err != nil {
						t.Fatal(err)
					}
				} else {
					retireDigestPair(t, second, first.MessageID, at.Add(31*24*time.Hour))
					close(release)
					got := <-done
					if point == "before lookup" {
						if !errors.Is(got.err, policystore.ErrRetryExpired) {
							t.Fatalf("stale snapshot: %+v", got)
						}
					} else if got.err != nil || got.ack.MessageID != first.MessageID {
						t.Fatalf("prior direct lookup: %+v", got)
					}
				}
				svc.DB = conn
				if _, err := digestSend(svc, kind, ctx, publisher(), cid, client, "secret"); !errors.Is(err, policystore.ErrRetryExpired) {
					t.Fatalf("post retirement: %v", err)
				}
				assertDigestNoNewWrites(t, conn, cid, 1)
			})
		}
	}
}
