package policystore_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

// These regressions catch an ACK committed after a real database lock wait
// crosses a membership or policy time boundary. Only the application clock is
// controlled; transactions, locks, writes, rollback and audits use PostgreSQL.
func textCommitFixture(t *testing.T, group bool) (*pgx.Conn, policystore.Service, string) {
	t.Helper()
	c := db(t)
	s := policystore.Service{DB: c, Now: func() time.Time { return at }}
	if !group {
		seedDirectConversation(t, c)
		return c, s, directA
	}
	seed(t, c)
	g, e := s.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	return c, s, g.ID
}

func textCommitSend(s policystore.Service, ctx context.Context, group bool, cid, key string) (policystore.MessageACK, error) {
	if group {
		return s.SendGroupTextMessage(ctx, publisher(), cid, key, "time boundary")
	}
	return s.SendTextMessage(ctx, publisher(), cid, key, "time boundary")
}

func textCommitBoundary(t *testing.T, c *pgx.Conn, s policystore.Service, boundary string) {
	t.Helper()
	switch boundary {
	case "actor_expires":
		run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at.Add(time.Second))
	case "target_expires":
		run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", targetM2, at.Add(time.Second))
	case "hard_deny_activates", "exception_expires":
		grantPublisher(t, c)
		rules := []policy.Rule{{ID: "commit-time-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(time.Second), Reason: "scheduled denial"}}
		if boundary == "exception_expires" {
			rules = []policy.Rule{
				{ID: "commit-time-isolate", TenantID: tenantA, Effect: policy.EffectIsolate, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, Bidirectional: true, EffectiveFrom: at.Add(-time.Hour), Reason: "isolated"},
				{ID: "commit-time-exception", TenantID: tenantA, Effect: policy.EffectExceptionAllow, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: adminM, TargetMembershipID: targetM2, Bidirectional: true, OverrideRuleID: "commit-time-isolate", RequestedBy: adminA, ApprovedBy: adminA, EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Second), Reason: "temporary exception"},
			}
		}
		if _, e := s.Publish(context.Background(), publisher(), 0, rules, "commit time regression"); e != nil {
			t.Fatal(e)
		}
	}
}

func textCommitBlocker(t *testing.T, c *pgx.Conn, phase string) pgx.Tx {
	t.Helper()
	if phase == "rate_lock" {
		run(t, c, "INSERT INTO message_rate_windows(tenant_id,sender_user_id,window_start,sent_count) VALUES($1,$2,$3,1)", tenantA, adminA, at.Add(-time.Second))
	} else {
		run(t, c, "CREATE TABLE text_commit_gate(id integer PRIMARY KEY); INSERT INTO text_commit_gate VALUES(1)")
		run(t, c, `CREATE FUNCTION text_commit_gate_wait() RETURNS trigger LANGUAGE plpgsql AS $$
  BEGIN IF NEW.action='message_send' AND NEW.outcome='allow' THEN
   PERFORM id FROM text_commit_gate WHERE id=1 FOR UPDATE;
  END IF; RETURN NEW; END $$`)
		run(t, c, "CREATE TRIGGER text_commit_gate_wait BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION text_commit_gate_wait()")
	}
	tx, e := filePeer(t, c).Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	query := "SELECT id FROM text_commit_gate WHERE id=1 FOR UPDATE"
	if phase == "rate_lock" {
		query = "SELECT sender_user_id FROM message_rate_windows FOR UPDATE"
	}
	if _, e = tx.Exec(context.Background(), query); e != nil {
		t.Fatal(e)
	}
	return tx
}

func assertTextCommitState(t *testing.T, c *pgx.Conn, cid string, want, wantRate int) {
	t.Helper()
	for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
		var n int
		if e := c.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", cid).Scan(&n); e != nil || n != want {
			t.Fatal(table, n, want, e)
		}
	}
	var seq, rate int
	if e := c.QueryRow(context.Background(), "SELECT last_seq FROM conversations WHERE id=$1", cid).Scan(&seq); e != nil || seq != want {
		t.Fatal("sequence", seq, want, e)
	}
	if e := c.QueryRow(context.Background(), "SELECT COALESCE(sum(sent_count),0) FROM message_rate_windows WHERE tenant_id=$1 AND sender_user_id=$2", tenantA, adminA).Scan(&rate); e != nil || rate != wantRate {
		t.Fatal("rate", rate, wantRate, e)
	}
}

func TestTextSendRechecksTimeAfterDatabaseLock(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, phase := range []string{"rate_lock", "final_audit_lock"} {
			for _, boundary := range []string{"actor_expires", "target_expires", "hard_deny_activates", "exception_expires"} {
				t.Run(map[bool]string{false: "direct", true: "group"}[group]+"/"+phase+"/"+boundary, func(t *testing.T) {
					c, s, cid := textCommitFixture(t, group)
					textCommitBoundary(t, c, s, boundary)
					blocker := textCommitBlocker(t, c, phase)
					sender := filePeer(t, c)
					var later atomic.Bool
					s.DB = sender
					s.Now = func() time.Time {
						if later.Load() {
							return at.Add(2 * time.Second)
						}
						return at
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					done := make(chan fileSendOutcome, 1)
					key := clientUUIDv7(at, 9701)
					go func() { ack, e := textCommitSend(s, ctx, group, cid, key); done <- fileSendOutcome{ack, e} }()
					waitFileSendLock(t, c, sender.PgConn().PID())
					later.Store(true)
					if e := blocker.Commit(context.Background()); e != nil {
						t.Fatal(e)
					}
					v := fileSendResult(t, done)
					want := policystore.ErrMessageNotAvailable
					if group {
						want = policystore.ErrGroupPolicyBlocked
					}
					if boundary == "actor_expires" {
						want = policystore.ErrForbidden
					}
					if !errors.Is(v.err, want) || v.ack.MessageID != "" {
						t.Fatalf("expired authority returned ACK: %+v err=%v want=%v", v.ack, v.err, want)
					}
					wantRate := 0
					if phase == "rate_lock" {
						wantRate = 1
					}
					assertTextCommitState(t, c, cid, 0, wantRate)
					if phase == "rate_lock" {
						var window time.Time
						if e := c.QueryRow(context.Background(), "SELECT window_start FROM message_rate_windows WHERE tenant_id=$1 AND sender_user_id=$2", tenantA, adminA).Scan(&window); e != nil || !window.Equal(at.Add(-time.Second)) {
							t.Fatal("provisional rate window survived", window, e)
						}
					}
					var allowed, denied int
					if e := c.QueryRow(context.Background(), "SELECT count(*) FILTER(WHERE outcome='allow'),count(*) FILTER(WHERE outcome='deny') FROM audit_events WHERE action='message_send'").Scan(&allowed, &denied); e != nil || allowed != 0 || denied != 1 {
						t.Fatal("request audits", allowed, denied, e)
					}
					if group && boundary != "actor_expires" {
						var status string
						if e := c.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", cid).Scan(&status); e != nil || status != "policy_blocked" {
							t.Fatal("group state", status, e)
						}
					}
					if boundary == "actor_expires" {
						run(t, c, "UPDATE user_organizations SET effective_to=NULL WHERE id=$1", adminM)
						ack, e := textCommitSend(s, context.Background(), group, cid, key)
						if e != nil || ack.Seq != 1 || ack.Duplicate {
							t.Fatal("same key after rejected provisional write", ack, e)
						}
						assertTextCommitState(t, c, cid, 1, 1)
					}
				})
			}
		}
	}
}

func TestTextSendCommitsAfterLockWithoutAuthorizationChange(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, phase := range []string{"rate_lock", "final_audit_lock"} {
			t.Run(map[bool]string{false: "direct", true: "group"}[group]+"/"+phase, func(t *testing.T) {
				c, s, cid := textCommitFixture(t, group)
				blocker := textCommitBlocker(t, c, phase)
				sender := filePeer(t, c)
				var later atomic.Bool
				s.DB = sender
				s.Now = func() time.Time {
					if later.Load() {
						return at.Add(2 * time.Second)
					}
					return at
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				done := make(chan fileSendOutcome, 1)
				key := clientUUIDv7(at, 9702)
				go func() { ack, e := textCommitSend(s, ctx, group, cid, key); done <- fileSendOutcome{ack, e} }()
				waitFileSendLock(t, c, sender.PgConn().PID())
				later.Store(true)
				if e := blocker.Commit(context.Background()); e != nil {
					t.Fatal(e)
				}
				v := fileSendResult(t, done)
				if v.err != nil || v.ack.Seq != 1 || v.ack.MessageID == "" {
					t.Fatal(v.ack, v.err)
				}
				assertTextCommitState(t, c, cid, 1, 1)
				replay, e := textCommitSend(s, context.Background(), group, cid, key)
				if e != nil || !replay.Duplicate || replay.MessageID != v.ack.MessageID {
					t.Fatal("durable replay", replay, e)
				}
				assertTextCommitState(t, c, cid, 1, 1)
			})
		}
	}
}

func TestTextReplayRechecksActorAfterFinalAuditWait(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "group"}[group], func(t *testing.T) {
			c, s, cid := textCommitFixture(t, group)
			key := clientUUIDv7(at, 9703)
			first, e := textCommitSend(s, context.Background(), group, cid, key)
			if e != nil {
				t.Fatal(e)
			}
			run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at.Add(time.Second))
			blocker := textCommitBlocker(t, c, "final_audit_lock")
			sender := filePeer(t, c)
			var later atomic.Bool
			s.DB = sender
			s.Now = func() time.Time {
				if later.Load() {
					return at.Add(2 * time.Second)
				}
				return at
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan fileSendOutcome, 1)
			go func() { ack, e := textCommitSend(s, ctx, group, cid, key); done <- fileSendOutcome{ack, e} }()
			waitFileSendLock(t, c, sender.PgConn().PID())
			later.Store(true)
			if e := blocker.Commit(context.Background()); e != nil {
				t.Fatal(e)
			}
			v := fileSendResult(t, done)
			if !errors.Is(v.err, policystore.ErrForbidden) || v.ack.MessageID != "" {
				t.Fatal("expired actor replayed ACK", first, v.ack, v.err)
			}
			assertTextCommitState(t, c, cid, 1, 1)
			var replayAllows, denied int
			if e := c.QueryRow(context.Background(), "SELECT count(*) FILTER(WHERE reason='idempotent_replay' AND outcome='allow'),count(*) FILTER(WHERE outcome='deny') FROM audit_events WHERE action='message_send'").Scan(&replayAllows, &denied); e != nil || replayAllows != 0 || denied != 1 {
				t.Fatal("replay audits", replayAllows, denied, e)
			}
		})
	}
}
