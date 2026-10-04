package policystore_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type fileSendOutcome struct {
	ack policystore.MessageACK
	err error
}

func fileSend(s policystore.Service, ctx context.Context, cid string, req policystore.MessageSendRequest, group bool) (policystore.MessageACK, error) {
	if group {
		return s.SendGroupMessage(ctx, publisher(), cid, req)
	}
	return s.SendMessage(ctx, publisher(), cid, req)
}
func concurrentFileFixture(t *testing.T, group bool) (*pgx.Conn, policystore.Service, string, files.Metadata, policystore.MessageSendRequest) {
	t.Helper()
	if group {
		return groupFileFixture(t)
	}
	c, s, m, r := directFileFixture(t)
	return c, s, directA, m, r
}
func waitFileSendLock(t *testing.T, c *pgx.Conn, pid uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var wait *string
		if err := c.QueryRow(ctx, "SELECT wait_event_type FROM pg_stat_activity WHERE pid=$1", pid).Scan(&wait); err != nil {
			t.Fatal(err)
		}
		if wait != nil && *wait == "Lock" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("sender never waited", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
func fileSendResult(t *testing.T, ch <-chan fileSendOutcome) fileSendOutcome {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(6 * time.Second):
		t.Fatal("send did not finish")
		return fileSendOutcome{}
	}
}
func TestFileMessageConcurrentSameKey(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "group"}[group], func(t *testing.T) {
			c, _, cid, _, req := concurrentFileFixture(t, group)
			peers := make([]*pgx.Conn, 8)
			for i := range peers {
				peers[i] = filePeer(t, c)
			}
			start := make(chan struct{})
			done := make(chan fileSendOutcome, 8)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, p := range peers {
				go func(conn *pgx.Conn) {
					<-start
					a, e := fileSend(policystore.Service{DB: conn, Now: func() time.Time { return at }}, ctx, cid, req, group)
					done <- fileSendOutcome{a, e}
				}(p)
			}
			close(start)
			fresh, duplicates := 0, 0
			var original policystore.MessageACK
			for range peers {
				v := fileSendResult(t, done)
				if v.err != nil {
					t.Fatal(v.err)
				}
				if v.ack.Duplicate {
					duplicates++
				} else {
					fresh++
					original = v.ack
				}
			}
			if fresh != 1 || duplicates != 7 {
				t.Fatal(fresh, duplicates)
			}
			assertFileMessageWrites(t, c, cid, 1)
			var id string
			if e := c.QueryRow(ctx, "SELECT id::text FROM messages WHERE conversation_id=$1", cid).Scan(&id); e != nil || id != original.MessageID {
				t.Fatal(id, original, e)
			}
		})
	}
}
func TestFileMessageConcurrentSameFile(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "group"}[group], func(t *testing.T) {
			c, _, cid, _, req := concurrentFileFixture(t, group)
			p1, p2 := filePeer(t, c), filePeer(t, c)
			start := make(chan struct{})
			done := make(chan fileSendOutcome, 2)
			for i, p := range []*pgx.Conn{p1, p2} {
				r := req
				r.ClientMessageID = clientUUIDv7(at, 8500+i)
				go func(conn *pgx.Conn, r policystore.MessageSendRequest) {
					<-start
					a, e := fileSend(policystore.Service{DB: conn, Now: func() time.Time { return at }}, context.Background(), cid, r, group)
					done <- fileSendOutcome{a, e}
				}(p, r)
			}
			close(start)
			success, denied := 0, 0
			for range 2 {
				v := fileSendResult(t, done)
				if v.err == nil {
					success++
				} else if errors.Is(v.err, policystore.ErrFileNotBindable) {
					denied++
				} else {
					t.Fatal(v.err)
				}
			}
			if success != 1 || denied != 1 {
				t.Fatal(success, denied)
			}
			assertFileMessageWrites(t, c, cid, 1)
		})
	}
}
func TestFileMessageDeleteRace(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "group"}[group], func(t *testing.T) {
			c, _, cid, m, req := concurrentFileFixture(t, group)
			blocker, sender := filePeer(t, c), filePeer(t, c)
			tx, e := blocker.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			_, e = tx.Exec(context.Background(), "UPDATE file_objects SET state='delete_pending',state_version=state_version+1,deletion_requested_at=$2,updated_at=$2 WHERE id=$1", m.ID, m.UpdatedAt.Add(time.Second))
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan fileSendOutcome, 1)
			go func() {
				a, e := fileSend(policystore.Service{DB: sender, Now: func() time.Time { return at }}, context.Background(), cid, req, group)
				done <- fileSendOutcome{a, e}
			}()
			waitFileSendLock(t, c, sender.PgConn().PID())
			if e = tx.Commit(context.Background()); e != nil {
				t.Fatal(e)
			}
			v := fileSendResult(t, done)
			if !errors.Is(v.err, policystore.ErrFileNotBindable) {
				t.Fatal(v)
			}
			assertFileMessageWrites(t, c, cid, 0)
		})
	}
}
func TestFileMessageConfigWait(t *testing.T) {
	for _, change := range []string{"disable", "lower limit", "type removed"} {
		t.Run(change, func(t *testing.T) {
			c, s, _, req := directFileFixture(t)
			m := freshFile()
			m.DeclaredSizeBytes = 2
			d, e := files.CreationDigest(m.CreateParams)
			if e != nil {
				t.Fatal(e)
			}
			m.RequestDigest = d[:]
			if e = writeFile(c, m, true); e != nil {
				t.Fatal(e)
			}
			for _, state := range []string{"uploaded", "scanning", "ready"} {
				m = fileNext(m, state)
				if e = writeFile(c, m, false); e != nil {
					t.Fatal(e)
				}
			}
			req.FileID = m.ID
			blocker, sender := filePeer(t, c), filePeer(t, c)
			tx, e := blocker.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			sql := "enabled=false"
			if change == "lower limit" {
				sql = "max_size_bytes=1"
			}
			if change == "type removed" {
				sql = "allowed_media_types=ARRAY['text/plain']"
			}
			if _, e = tx.Exec(context.Background(), "UPDATE tenant_file_upload_policy SET "+sql+" WHERE tenant_id=$1", tenantA); e != nil {
				t.Fatal(e)
			}
			s.DB = sender
			done := make(chan fileSendOutcome, 1)
			go func() {
				a, e := s.SendMessage(context.Background(), publisher(), directA, req)
				done <- fileSendOutcome{a, e}
			}()
			waitFileSendLock(t, c, sender.PgConn().PID())
			if e = tx.Commit(context.Background()); e != nil {
				t.Fatal(e)
			}
			v := fileSendResult(t, done)
			if !errors.Is(v.err, policystore.ErrFileNotBindable) {
				t.Fatal(v)
			}
			assertFileMessageWrites(t, c, directA, 0)
		})
	}
}
func TestFileMessageMembershipWait(t *testing.T) {
	for _, which := range []string{"frozen user", "ended membership", "group removed"} {
		t.Run(which, func(t *testing.T) {
			group := which == "group removed"
			c, s, cid, _, req := concurrentFileFixture(t, group)
			blocker, sender := filePeer(t, c), filePeer(t, c)
			tx, e := blocker.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			switch which {
			case "frozen user":
				_, e = tx.Exec(context.Background(), "UPDATE users SET status='frozen' WHERE id=$1", adminA)
			case "ended membership":
				_, e = tx.Exec(context.Background(), "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at)
			case "group removed":
				_, e = tx.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", cid)
				if e == nil {
					_, e = tx.Exec(context.Background(), "UPDATE conversation_membership_intervals SET status='removed',leave_seq=0,left_at=$2 WHERE conversation_id=$1 AND user_id=$3 AND status='active'", cid, at, adminA)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			s.DB = sender
			done := make(chan fileSendOutcome, 1)
			go func() { a, e := fileSend(s, context.Background(), cid, req, group); done <- fileSendOutcome{a, e} }()
			if which == "ended membership" {
				v := fileSendResult(t, done)
				var pe *pgconn.PgError
				if !errors.As(v.err, &pe) || pe.Code != "55P03" {
					t.Fatal("membership NOWAIT did not fail closed", v)
				}
				if e = tx.Commit(context.Background()); e != nil {
					t.Fatal(e)
				}
				a, e := fileSend(s, context.Background(), cid, req, group)
				if !errors.Is(e, policystore.ErrForbidden) || a.MessageID != "" {
					t.Fatal(a, e)
				}
				assertFileMessageWrites(t, c, cid, 0)
				return
			}
			waitFileSendLock(t, c, sender.PgConn().PID())
			if e = tx.Commit(context.Background()); e != nil {
				t.Fatal(e)
			}
			v := fileSendResult(t, done)
			if v.err == nil {
				t.Fatal("changed authority sent", v)
			}
			assertFileMessageWrites(t, c, cid, 0)
		})
	}
}
func TestFileMessageAuditWait(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, which := range []string{"membership expires", "rule activates"} {
			t.Run(map[bool]string{false: "direct", true: "group"}[group]+"/"+which, func(t *testing.T) {
				c, s, cid, _, req := concurrentFileFixture(t, group)
				if which == "membership expires" {
					run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at.Add(time.Second))
				} else {
					grantPublisher(t, c)
					rule := policy.Rule{ID: "file-wait-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "wait"}
					if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "wait"); e != nil {
						t.Fatal(e)
					}
				}
				blocker, sender := filePeer(t, c), filePeer(t, c)
				tx, e := blocker.Begin(context.Background())
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(context.Background())
				if _, e = tx.Exec(context.Background(), "LOCK TABLE audit_events IN SHARE MODE"); e != nil {
					t.Fatal(e)
				}
				var later atomic.Bool
				s.DB = sender
				s.Now = func() time.Time {
					if later.Load() {
						return at.Add(2 * time.Second)
					}
					return at
				}
				done := make(chan fileSendOutcome, 1)
				go func() { a, e := fileSend(s, context.Background(), cid, req, group); done <- fileSendOutcome{a, e} }()
				waitFileSendLock(t, c, sender.PgConn().PID())
				later.Store(true)
				if e = tx.Commit(context.Background()); e != nil {
					t.Fatal(e)
				}
				v := fileSendResult(t, done)
				if v.err == nil {
					t.Fatal("expired send acknowledged", v)
				}
				assertFileMessageWrites(t, c, cid, 0)
				var allow int
				if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&allow); e != nil || allow != 0 {
					t.Fatal("success audit survived", allow, e)
				}
				if group && which == "rule activates" {
					var status string
					if e = c.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", cid).Scan(&status); e != nil || status != "policy_blocked" {
						t.Fatal(status, e)
					}
				}
			})
		}
	}
}
func TestFileMessageFaultRollback(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, table := range []string{"messages", "message_attachments", "message_idempotency", "outbox_events", "audit_events"} {
			t.Run(map[bool]string{false: "direct", true: "group"}[group]+"/"+table, func(t *testing.T) {
				c, s, cid, _, req := concurrentFileFixture(t, group)
				run(t, c, `CREATE FUNCTION fail_file_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'file fault'; END $$`)
				run(t, c, "CREATE TRIGGER fail_file_fixture BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION fail_file_fixture()")
				a, e := fileSend(s, context.Background(), cid, req, group)
				if e == nil || a.MessageID != "" {
					t.Fatal(a, e)
				}
				assertFileMessageWrites(t, c, cid, 0)
				run(t, c, "DROP TRIGGER fail_file_fixture ON "+table)
				a, e = fileSend(s, context.Background(), cid, req, group)
				if e != nil || a.Seq != 1 {
					t.Fatal(a, e)
				}
				assertFileMessageWrites(t, c, cid, 1)
			})
		}
	}
}
func retireFileProofTx(t *testing.T, tx pgx.Tx, mid string) {
	t.Helper()
	stamp := at.Add(31 * 24 * time.Hour)
	for _, q := range []string{"UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", "UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$2 WHERE message_id=$1"} {
		if _, e := tx.Exec(context.Background(), q, mid, stamp); e != nil {
			t.Fatal(e)
		}
	}
}
func TestFileMessageReplayRetirementRace(t *testing.T) {
	t.Run("retirement commits before replay lock", func(t *testing.T) {
		c, s, _, req := directFileFixture(t)
		first, e := s.SendMessage(context.Background(), publisher(), directA, req)
		if e != nil {
			t.Fatal(e)
		}
		peer, sender := filePeer(t, c), filePeer(t, c)
		tx, e := peer.Begin(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		if _, e = tx.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", directA); e != nil {
			t.Fatal(e)
		}
		retireFileProofTx(t, tx, first.MessageID)
		s.DB = sender
		done := make(chan fileSendOutcome, 1)
		go func() {
			a, e := s.SendMessage(context.Background(), publisher(), directA, req)
			done <- fileSendOutcome{a, e}
		}()
		waitFileSendLock(t, c, sender.PgConn().PID())
		if e = tx.Commit(context.Background()); e != nil {
			t.Fatal(e)
		}
		v := fileSendResult(t, done)
		if !errors.Is(v.err, policystore.ErrRetryExpired) {
			t.Fatal(v)
		}
		assertFileMessageWrites(t, c, directA, 1)
	})
	t.Run("replay commits before retirement lock", func(t *testing.T) {
		c, s, _, req := directFileFixture(t)
		first, e := s.SendMessage(context.Background(), publisher(), directA, req)
		if e != nil {
			t.Fatal(e)
		}
		sender, retirer := filePeer(t, c), filePeer(t, c)
		reached, proceed := make(chan struct{}), make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(proceed) }) }
		defer release()
		s.DB = afterFileAuditDB{Beginner: sender, after: func() { close(reached); <-proceed }}
		done := make(chan fileSendOutcome, 1)
		go func() {
			a, e := s.SendMessage(context.Background(), publisher(), directA, req)
			done <- fileSendOutcome{a, e}
		}()
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatal("replay audit not reached")
		}
		tx, e := retirer.Begin(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		lockDone := make(chan error, 1)
		go func() {
			_, e := tx.Exec(context.Background(), "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", directA)
			lockDone <- e
		}()
		waitFileSendLock(t, c, retirer.PgConn().PID())
		release()
		v := fileSendResult(t, done)
		if v.err != nil || !v.ack.Duplicate || v.ack.MessageID != first.MessageID {
			t.Fatal(v)
		}
		if e = <-lockDone; e != nil {
			t.Fatal(e)
		}
		retireFileProofTx(t, tx, first.MessageID)
		if e = tx.Commit(context.Background()); e != nil {
			t.Fatal(e)
		}
		assertFileMessageWrites(t, c, directA, 1)
	})
}
