package policystore_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fileHistoryFixture(t *testing.T, kind string) (*pgx.Conn, policystore.Service, string, files.Metadata) {
	t.Helper()
	var c *pgx.Conn
	var s policystore.Service
	var cid string
	var m files.Metadata
	if kind == "group" {
		c, s, cid, m, _ = groupFileFixture(t)
	} else {
		c, s, m, _ = directFileFixture(t)
		cid = directA
	}
	send := s.SendMessage
	if kind == "group" {
		send = s.SendGroupMessage
	}
	for i, req := range []policystore.MessageSendRequest{{MessageType: "text", Text: "match first"}, {MessageType: "file", FileID: m.ID, Caption: "match caption"}, {MessageType: "text", Text: "match last"}} {
		req.ClientMessageID = clientUUIDv7(at, 8300+i)
		if _, e := send(context.Background(), publisher(), cid, req); e != nil {
			t.Fatal(e)
		}
	}
	return c, s, cid, m
}
func assertFileHistory(t *testing.T, kind string) {
	t.Helper()
	_, s, cid, m := fileHistoryFixture(t, kind)
	pull := s.PullTextMessages
	if kind == "group" {
		pull = s.PullGroupTextMessages
	}
	after := int64(0)
	for seq := int64(1); seq <= 3; seq++ {
		page, e := pull(context.Background(), publisher(), cid, after, 1)
		if e != nil || len(page.Messages) != 1 || page.NextAfterSeq != seq || page.HasMore != (seq < 3) {
			t.Fatal(page, e)
		}
		got := page.Messages[0]
		if got.Seq != seq || got.Redacted {
			t.Fatal(got)
		}
		if seq == 2 {
			a := got.Attachment
			if got.MessageType != "file" || got.Text != "match caption" || a == nil || a.FileID != m.ID || !a.Available || a.OriginalFilename != m.OriginalFilename || a.ActualSizeBytes == nil || *a.ActualSizeBytes != 1 || a.DetectedMediaType != "application/pdf" {
				t.Fatal(got, a)
			}
		} else if got.MessageType != "text" || got.Attachment != nil {
			t.Fatal(got)
		}
		after = seq
	}
}
func TestFileMessageHistoryDirect(t *testing.T) { assertFileHistory(t, "direct") }
func TestFileMessageHistoryGroup(t *testing.T)  { assertFileHistory(t, "group") }
func TestFileMessageHistoryRedaction(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		for _, why := range []string{"cleared", "retention", "hard deny", "final time"} {
			t.Run(kind+"/"+why, func(t *testing.T) {
				c, s, cid, _ := fileHistoryFixture(t, kind)
				pull := s.PullTextMessages
				if kind == "group" {
					pull = s.PullGroupTextMessages
				}
				switch why {
				case "cleared":
					run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE conversation_id=$1 AND seq=2", cid, at)
				case "retention":
					s.Now = func() time.Time { return at.Add(366 * 24 * time.Hour) }
				case "hard deny":
					grantPublisher(t, c)
					r := policy.Rule{ID: "history-stop", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Second), Reason: "history hidden"}
					if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{r}, "hidden"); e != nil {
						t.Fatal(e)
					}
				case "final time":
					var clock atomic.Int64
					clock.Store(at.UnixNano())
					s.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
					s.DB = damagedHistoryDB{Beginner: c, after: func() { clock.Store(at.Add(366 * 24 * time.Hour).UnixNano()) }}
				}
				// Refresh method value after changing the service clock.
				pull = s.PullTextMessages
				if kind == "group" {
					pull = s.PullGroupTextMessages
				}
				page, e := pull(context.Background(), publisher(), cid, 1, 1)
				if e != nil || len(page.Messages) != 1 {
					t.Fatal(page, e)
				}
				want := policystore.PulledMessage{Seq: 2, Redacted: true}
				if page.Messages[0] != want {
					t.Fatal("card or caption survived whole-message redaction", page.Messages[0])
				}
			})
		}
	}
}
func TestFileMessageHistoryUnavailable(t *testing.T) {
	for _, state := range []string{"delete_pending", "deleted", "incomplete metadata", "missing association", "damaged association"} {
		t.Run(state, func(t *testing.T) {
			c, s, cid, m := fileHistoryFixture(t, "direct")
			if state == "delete_pending" || state == "deleted" {
				m = fileNext(m, "delete_pending")
				if e := writeFile(c, m, false); e != nil {
					t.Fatal(e)
				}
				if state == "deleted" {
					m = fileNext(m, "deleted")
					if e := writeFile(c, m, false); e != nil {
						t.Fatal(e)
					}
				}
			} else {
				s.DB = damagedHistoryDB{Beginner: c, empty: state == "damaged association", metadata: state == "incomplete metadata"}
			}
			page, e := s.PullTextMessages(context.Background(), publisher(), cid, 1, 1)
			if e != nil || len(page.Messages) != 1 {
				t.Fatal(page, e)
			}
			item := page.Messages[0]
			if strings.Contains(state, "association") {
				if item != (policystore.PulledMessage{Seq: 2, Redacted: true}) {
					t.Fatal("bad association not closed", item)
				}
				return
			}
			if item.Redacted || item.Attachment == nil || item.Attachment.FileID != m.ID || item.Attachment.Available || item.Attachment.OriginalFilename != "" || item.Attachment.ActualSizeBytes != nil || item.Attachment.DetectedMediaType != "" {
				t.Fatal(item, item.Attachment)
			}
		})
	}
}

type damagedHistoryDB struct {
	access.Beginner
	empty    bool
	metadata bool
	after    func()
}

func (d damagedHistoryDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	return damagedHistoryTx{Tx: tx, empty: d.empty, metadata: d.metadata, after: d.after}, e
}

type damagedHistoryTx struct {
	pgx.Tx
	empty    bool
	metadata bool
	after    func()
}

func (tx damagedHistoryTx) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	rows, e := tx.Tx.Query(ctx, q, args...)
	if e == nil && strings.Contains(q, "FROM messages m") {
		return damagedHistoryRows{Rows: rows, empty: tx.empty, metadata: tx.metadata, after: tx.after}, nil
	}
	return rows, e
}

type damagedHistoryRows struct {
	pgx.Rows
	empty    bool
	metadata bool
	after    func()
}

func (r damagedHistoryRows) Scan(dest ...any) error {
	if e := r.Rows.Scan(dest...); e != nil {
		return e
	}
	if r.after == nil && len(dest) >= 6 {
		if kind, ok := dest[len(dest)-6].(*string); ok && *kind == "file" {
			if r.metadata {
				*(dest[len(dest)-3].(**string)) = nil
				return nil
			}
			file := dest[len(dest)-5].(**string)
			*file = nil
			if r.empty {
				v := ""
				*file = &v
			}
		}
	}
	return nil
}

func (r damagedHistoryRows) Close() {
	r.Rows.Close()
	if r.after != nil {
		r.after()
	}
}

func TestFileMessageHistoryEmptyCaption(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			var c *pgx.Conn
			var s policystore.Service
			var cid string
			var req policystore.MessageSendRequest
			if kind == "group" {
				c, s, cid, _, req = groupFileFixture(t)
			} else {
				c, s, _, req = directFileFixture(t)
				cid = directA
			}
			_ = c
			req.Caption = ""
			send := s.SendMessage
			pull := s.PullTextMessages
			if kind == "group" {
				send = s.SendGroupMessage
				pull = s.PullGroupTextMessages
			}
			if _, e := send(context.Background(), publisher(), cid, req); e != nil {
				t.Fatal(e)
			}
			page, e := pull(context.Background(), publisher(), cid, 0, 1)
			if e != nil || len(page.Messages) != 1 {
				t.Fatal(page, e)
			}
			m := page.Messages[0]
			if m.Redacted || m.MessageType != "file" || m.Text != "" || m.Attachment == nil || !m.Attachment.Available {
				t.Fatal("empty caption treated as absent body", m)
			}
		})
	}
}
