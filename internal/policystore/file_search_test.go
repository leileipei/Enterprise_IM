package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
)

func namedSearchFile(t *testing.T, c *pgx.Conn, cid, name string) files.Metadata {
	t.Helper()
	m := freshFile()
	m.ConversationID = cid
	m.UploaderUserID = adminA
	m.UploaderMembershipID = adminM
	m.OriginalFilename = name
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
	return m
}
func TestFileSearchLiteral(t *testing.T) {
	c, s, _, req := directFileFixture(t)
	m := namedSearchFile(t, c, directA, "报告 AbC%_中文.pdf")
	req.FileID = m.ID
	req.Caption = "caption-only secret"
	if _, e := s.SendMessage(context.Background(), publisher(), directA, req); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"  ABC%_  ", "中文", "%_"} {
		p, e := s.SearchFileMessages(context.Background(), groupMemberIdentity(), directA, "direct", q, "", 20)
		if e != nil || len(p.Matches) != 1 || p.Matches[0].FileID != m.ID || p.Matches[0].OriginalFilename != m.OriginalFilename {
			t.Fatal(q, p, e)
		}
	}
	for _, q := range []string{"caption-only", "secret", "message body", "[.*]"} {
		p, e := s.SearchFileMessages(context.Background(), groupMemberIdentity(), directA, "direct", q, "", 20)
		if e != nil || len(p.Matches) != 0 {
			t.Fatal("matched content", q, p, e)
		}
	}
}
func TestFileSearchDirectAndGroup(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			c, s, cid, m := fileHistoryFixture(t, kind)
			p, e := s.SearchFileMessages(context.Background(), groupMemberIdentity(), strings.ToUpper(cid), kind, m.OriginalFilename, "", 20)
			if e != nil || len(p.Matches) != 1 || p.Matches[0].Seq != 2 || p.Matches[0].Kind != kind || p.HasMore || p.NextCursor != "" {
				t.Fatal(p, e)
			}
			var sessions int
			if e = c.QueryRow(context.Background(), "SELECT count(*) FROM file_download_sessions").Scan(&sessions); e != nil || sessions != 0 {
				t.Fatal("search allocated download", sessions, e)
			}
			run(t, c, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", cid)
			p, e = s.SearchFileMessages(context.Background(), groupMemberIdentity(), cid, kind, m.OriginalFilename, "", 20)
			if e != nil || len(p.Matches) != 0 {
				t.Fatal("blocked name", p, e)
			}
		})
	}
}
func TestFileSearchBoundedProgress(t *testing.T) {
	c, s, _, req := directFileFixture(t)
	ctx := context.Background()
	if _, e := s.SendMessage(ctx, publisher(), directA, req); e != nil {
		t.Fatal(e)
	}
	// Inject missing links only in this owned schema: corrupted candidates must still consume budget.
	run(t, c, "ALTER TABLE messages DISABLE TRIGGER USER")
	for i := 2; i <= 501; i++ {
		run(t, c, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id,client_msg_id,message_type,text_body,content_digest,accepted_at) SELECT tenant_id,conversation_id,$1,sender_user_id,sender_membership_id,recipient_user_id,recipient_membership_id,sender_organization_id,recipient_organization_id,$2,message_type,text_body,content_digest,accepted_at FROM messages WHERE conversation_id=$3 AND seq=1`, int64(i), clientUUIDv7(at, 10000+i), directA)
	}
	run(t, c, "ALTER TABLE messages ENABLE TRIGGER USER")
	run(t, c, "UPDATE conversations SET last_seq=501 WHERE id=$1", directA)
	m := namedSearchFile(t, c, directA, "needle.pdf")
	req.ClientMessageID = clientUUIDv7(at, 14000)
	req.FileID = m.ID
	if _, e := s.SendMessage(ctx, publisher(), directA, req); e != nil {
		t.Fatal(e)
	}
	p, e := s.SearchFileMessages(ctx, publisher(), directA, "direct", "needle", "", 20)
	if e != nil || len(p.Matches) != 0 || !p.HasMore || p.NextCursor == "" {
		t.Fatal("empty bounded page", p, e)
	}
	p, e = s.SearchFileMessages(ctx, publisher(), directA, "direct", "needle", p.NextCursor, 20)
	if e != nil || len(p.Matches) != 1 || p.Matches[0].Seq != 502 || p.HasMore {
		t.Fatal("lost progress", p, e)
	}
}
func TestFileSearchAuditFailure(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "direct")
	run(t, c, `ALTER TABLE audit_events ADD CONSTRAINT reject_file_search CHECK(action<>'file_name_search') NOT VALID`)
	p, e := s.SearchFileMessages(context.Background(), publisher(), cid, "direct", m.OriginalFilename, "", 20)
	if !errors.Is(e, policystore.ErrAuditUnavailable) || len(p.Matches) != 0 {
		t.Fatal("audit released names", p, e)
	}
}
