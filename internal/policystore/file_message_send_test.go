package policystore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func directFileFixture(t *testing.T) (*pgx.Conn, policystore.Service, files.Metadata, policystore.MessageSendRequest) {
	t.Helper()
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	m := fileMessageFixture(t, c, directA, adminA, adminM)
	return c, policystore.Service{DB: c, Now: func() time.Time { return at }}, m, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 8101), MessageType: "file", FileID: m.ID, Caption: ""}
}
func assertFileMessageWrites(t *testing.T, c *pgx.Conn, cid string, want int) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"messages", "message_attachments", "message_idempotency", "outbox_events"} {
		var n int
		if e := c.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", cid).Scan(&n); e != nil || n != want {
			t.Fatal(table, n, want, e)
		}
	}
	var seq, rate int64
	if e := c.QueryRow(ctx, "SELECT last_seq FROM conversations WHERE id=$1", cid).Scan(&seq); e != nil || seq != int64(want) {
		t.Fatal("seq", seq, want, e)
	}
	if e := c.QueryRow(ctx, "SELECT COALESCE(sum(sent_count),0) FROM message_rate_windows WHERE tenant_id=$1 AND sender_user_id=$2", tenantA, adminA).Scan(&rate); e != nil || rate != int64(want) {
		t.Fatal("rate", rate, want, e)
	}
}
func TestFileMessageDirectAtomic(t *testing.T) {
	c, s, m, req := directFileFixture(t)
	req.Caption = "报告<&\r\n"
	ack, e := s.SendMessage(context.Background(), publisher(), directA, req)
	if e != nil || ack.MessageID == "" || ack.Duplicate || ack.Seq != 1 || ack.ConversationID != directA || !ack.ServerTime.Equal(at) {
		t.Fatal(ack, e)
	}
	assertFileMessageWrites(t, c, directA, 1)
	var kind, caption, recipient, member string
	var digest, fp []byte
	if e = c.QueryRow(context.Background(), `SELECT m.message_type,m.text_body,m.content_digest,a.sealed_sha256,m.recipient_user_id::text,m.recipient_membership_id::text FROM messages m JOIN message_attachments a ON a.message_id=m.id WHERE m.id=$1`, ack.MessageID).Scan(&kind, &caption, &digest, &fp, &recipient, &member); e != nil || kind != "file" || caption != req.Caption || !bytes.Equal(fp, m.SHA256) || recipient != personA || member != targetM2 {
		t.Fatal(kind, caption, recipient, member, e)
	}
	var sealed [32]byte
	copy(sealed[:], fp)
	want, e := policystore.FileMessageDigestForTest(publisher(), directA, req, sealed)
	if e != nil || !bytes.Equal(digest, want[:]) {
		t.Fatal("digest", e)
	}
	var audits int
	if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&audits); e != nil || audits != 1 {
		t.Fatal(audits, e)
	}
	text := policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 8102), MessageType: "text", Text: "original text"}
	textAck, e := s.SendMessage(context.Background(), publisher(), directA, text)
	if e != nil || textAck.Seq != 2 {
		t.Fatal(textAck, e)
	}
	if e = c.QueryRow(context.Background(), "SELECT message_type,content_digest FROM messages WHERE id=$1", textAck.MessageID).Scan(&kind, &digest); e != nil || kind != "text" {
		t.Fatal(kind, e)
	}
	raw := sha256.Sum256([]byte(text.Text))
	if !bytes.Equal(digest, raw[:]) {
		t.Fatal("legacy text digest changed")
	}
}
func TestFileMessageDirectReplay(t *testing.T) {
	c, s, m, req := directFileFixture(t)
	req.Caption = "original"
	first, e := s.SendMessage(context.Background(), publisher(), directA, req)
	if e != nil {
		t.Fatal(e)
	}
	check := func() {
		t.Helper()
		got, e := s.SendMessage(context.Background(), publisher(), directA, req)
		if e != nil || !got.Duplicate || got.MessageID != first.MessageID || got.Seq != first.Seq || !got.ServerTime.Equal(first.ServerTime) {
			t.Fatal(first, got, e)
		}
		assertFileMessageWrites(t, c, directA, 1)
	}
	check()
	changed := req
	changed.Caption = "changed"
	if _, e = s.SendMessage(context.Background(), publisher(), directA, changed); !errors.Is(e, policystore.ErrIdempotencyConflict) {
		t.Fatal(e)
	}
	if _, e = s.SendTextMessage(context.Background(), publisher(), directA, req.ClientMessageID, req.Caption); !errors.Is(e, policystore.ErrIdempotencyConflict) {
		t.Fatal("type conflict", e)
	}
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", first.MessageID, at)
	m = fileNext(m, "delete_pending")
	if e = writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	m = fileNext(m, "deleted")
	if e = writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1", tenantA)
	check()
	run(t, c, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
	if _, e = s.SendMessage(context.Background(), publisher(), directA, req); !errors.Is(e, policystore.ErrForbidden) {
		t.Fatal("frozen replay", e)
	}
}
func TestFileMessageDirectOrigin(t *testing.T) {
	for _, which := range []string{"missing", "other user", "other conversation", "other membership", "other tenant", "not ready", "already bound", "selection changed"} {
		t.Run(which, func(t *testing.T) {
			c, s, m, req := directFileFixture(t)
			want := policystore.ErrMessageNotAvailable
			switch which {
			case "missing":
				req.FileID = freshFile().ID
			case "other user":
				req.FileID = fileMessageFixture(t, c, directA, personA, targetM2).ID
			case "other conversation":
				group, e := s.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
				if e != nil {
					t.Fatal(e)
				}
				req.FileID = fileMessageFixture(t, c, group.ID, adminA, adminM).ID
			case "other membership":
				other := freshFile().ID
				run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", other, tenantA, adminA, orgA2)
				req.FileID = fileMessageFixture(t, c, directA, adminA, other).ID
			case "other tenant":
				id := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
				_, e := s.SendMessage(context.Background(), id, directA, req)
				if !errors.Is(e, want) {
					t.Fatal(e)
				}
				assertFileMessageWrites(t, c, directA, 0)
				return
			case "not ready":
				req.FileID = storedFile(t, c, "scanning").ID
				want = policystore.ErrFileNotBindable
			case "already bound":
				_, e := s.SendMessage(context.Background(), publisher(), directA, req)
				if e != nil {
					t.Fatal(e)
				}
				req.ClientMessageID = clientUUIDv7(at, 8102)
				want = policystore.ErrFileNotBindable
			case "selection changed":
				other := freshFile().ID
				run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", other, tenantA, adminA, orgA2)
				_, e := s.SendMessage(context.Background(), publisher(), directA, req)
				if e != nil {
					t.Fatal(e)
				}
				run(t, c, "UPDATE conversations SET direct_low_membership_id=$2 WHERE id=$1", directA, other)
				want = policystore.ErrConversationContextChanged
			}
			_, e := s.SendMessage(context.Background(), publisher(), directA, req)
			if !errors.Is(e, want) {
				t.Fatal(which, m.ID, e, want)
			}
			n := 0
			if which == "already bound" || which == "selection changed" {
				n = 1
			}
			assertFileMessageWrites(t, c, directA, n)
		})
	}
}
func TestFileMessageDirectPolicy(t *testing.T) {
	for _, which := range []string{"disabled", "declared type", "detected type", "size", "member expired during audit", "rule expired during audit"} {
		t.Run(which, func(t *testing.T) {
			c, s, _, req := directFileFixture(t)
			want := policystore.ErrFileNotBindable
			switch which {
			case "disabled":
				run(t, c, "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1", tenantA)
			case "declared type", "detected type", "size":
				m := freshFile()
				if which == "declared type" {
					m.DeclaredMediaType = "text/plain"
				}
				if which == "size" {
					m.DeclaredSizeBytes = 2
				}
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
					if state == "uploaded" && which == "detected type" {
						m.DetectedMediaType = "text/plain"
					}
					if e = writeFile(c, m, false); e != nil {
						t.Fatal(e)
					}
				}
				req.FileID = m.ID
				types := []string{"application/pdf"}
				run(t, c, "UPDATE tenant_file_upload_policy SET max_size_bytes=1,allowed_media_types=$2 WHERE tenant_id=$1", tenantA, types)
			case "member expired during audit":
				run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at.Add(time.Second))
				want = policystore.ErrForbidden
			case "rule expired during audit":
				run(t, c, "UPDATE conversations SET direct_high_membership_id=$2 WHERE id=$1", directA, targetM)
				grantPublisher(t, c)
				isolate := isolationRule()
				isolate.Action = policy.ActionSendMessage
				exception := exceptionRule()
				exception.Action = policy.ActionSendMessage
				exception.EffectiveTo = at.Add(time.Second)
				if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{isolate, exception}, "audit wait expiry"); e != nil {
					t.Fatal(e)
				}
				want = policystore.ErrMessageNotAvailable
			}
			if strings.Contains(which, "during audit") {
				var clock atomic.Int64
				clock.Store(at.UnixNano())
				s.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
				s.DB = afterFileAuditDB{Beginner: c, after: func() { clock.Store(at.Add(2 * time.Second).UnixNano()) }}
			}
			ack, e := s.SendMessage(context.Background(), publisher(), directA, req)
			if !errors.Is(e, want) || ack.MessageID != "" {
				t.Fatal(which, ack, e, want)
			}
			assertFileMessageWrites(t, c, directA, 0)
			var allow int
			if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&allow); e != nil || allow != 0 {
				t.Fatal("false success audit", allow, e)
			}
		})
	}
}

type afterFileAuditDB struct {
	access.Beginner
	after func()
}

func (d afterFileAuditDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	return afterFileAuditTx{Tx: tx, after: d.after}, e
}

type afterFileAuditTx struct {
	pgx.Tx
	after func()
}

func (tx afterFileAuditTx) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	tag, e := tx.Tx.Exec(ctx, q, args...)
	if e == nil && strings.Contains(q, "INSERT INTO audit_events") && strings.Contains(q, "'message_send'") && len(args) > 4 && args[4] == "allow" {
		tx.after()
	}
	return tag, e
}
func TestFileMessageDirectAuditRollback(t *testing.T) {
	for _, table := range []string{"audit_events", "outbox_events"} {
		t.Run(table, func(t *testing.T) {
			c, s, _, req := directFileFixture(t)
			run(t, c, `CREATE FUNCTION fail_file_message_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
			run(t, c, "CREATE TRIGGER fail_file_message_write BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION fail_file_message_write()")
			ack, e := s.SendMessage(context.Background(), publisher(), directA, req)
			if e == nil || ack.MessageID != "" {
				t.Fatal(ack, e)
			}
			if table == "audit_events" && !errors.Is(e, policystore.ErrAuditUnavailable) {
				t.Fatal(e)
			}
			assertFileMessageWrites(t, c, directA, 0)
		})
	}
}

func TestFileMessageDirectReplayAuditExpiry(t *testing.T) {
	c, s, _, req := directFileFixture(t)
	first, e := s.SendMessage(context.Background(), publisher(), directA, req)
	if e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", adminM, at.Add(time.Second))
	var clock atomic.Int64
	clock.Store(at.UnixNano())
	s.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	s.DB = afterFileAuditDB{Beginner: c, after: func() { clock.Store(at.Add(2 * time.Second).UnixNano()) }}
	replay, e := s.SendMessage(context.Background(), publisher(), directA, req)
	if !errors.Is(e, policystore.ErrForbidden) || replay.MessageID != "" {
		t.Fatal("replay acknowledged expired actor", first, replay, e)
	}
	assertFileMessageWrites(t, c, directA, 1)
	var successful int
	if e = c.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='message_send' AND outcome='allow'").Scan(&successful); e != nil || successful != 1 {
		t.Fatal("expired replay left success audit", successful, e)
	}
}
