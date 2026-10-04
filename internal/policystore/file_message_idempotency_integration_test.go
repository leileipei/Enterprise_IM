package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func TestFileMessageReplayEvidence(t *testing.T) {
	c := fileMessageDB(t)
	seedDirectConversation(t, c)
	m := fileMessageFixture(t, c, directA, adminA, adminM)
	req := policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 7001), MessageType: "file", FileID: m.ID, Caption: "original"}
	var fp [32]byte
	copy(fp[:], m.SHA256)
	digest, e := policystore.FileMessageDigestForTest(publisher(), directA, req, fp)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	mid := rawFileMessage(t, tx, m, 1, "file", req.Caption)
	if e = rawFileBinding(context.Background(), tx, m, mid, m.SHA256); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(context.Background(), "UPDATE messages SET content_digest=$2 WHERE id=$1", mid, digest[:]); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(context.Background(), "UPDATE message_idempotency SET content_digest=$2 WHERE message_id=$1", mid, digest[:]); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	check := func(request policystore.MessageSendRequest, wantSame bool, wantErr error) {
		t.Helper()
		tx, e := c.Begin(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		ack, exists, same, e := policystore.ExistingTypedMessageACKForTest(context.Background(), tx, publisher(), directA, request)
		if !exists || same != wantSame || !errors.Is(e, wantErr) {
			t.Fatal(ack, exists, same, e)
		}
		if e == nil && (ack.MessageID != mid || ack.Seq != 1 || !ack.ServerTime.Equal(at)) {
			t.Fatal("original ACK changed", ack)
		}
	}
	check(req, true, nil)
	changed := req
	changed.Caption = "changed"
	check(changed, false, nil)
	text := policystore.MessageSendRequest{ClientMessageID: req.ClientMessageID, MessageType: "text", Text: "original"}
	check(text, false, nil)
	run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=$2 WHERE id=$1", mid, at)
	m = fileNext(m, "delete_pending")
	if e = writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	m = fileNext(m, "deleted")
	if e = writeFile(c, m, false); e != nil {
		t.Fatal(e)
	}
	check(req, true, nil)
	run(t, c, "BEGIN")
	stamp := at.Add(31 * 24 * time.Hour)
	run(t, c, "UPDATE messages SET content_digest=NULL,digest_retired_at=$2 WHERE id=$1", mid, stamp)
	run(t, c, "UPDATE message_idempotency SET content_digest=NULL,digest_retired_at=$2 WHERE message_id=$1", mid, stamp)
	run(t, c, "UPDATE message_attachments SET sealed_sha256=NULL,fingerprint_retired_at=$2 WHERE message_id=$1", mid, stamp)
	run(t, c, "COMMIT")
	check(req, false, policystore.ErrRetryExpired)
}
