package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func reservationParams() files.CreateParams {
	return files.CreateParams{TenantID: tenantA, ConversationID: directA, UploaderUserID: adminA, UploaderMembershipID: adminM, UploadRequestID: clientA, OriginalFilename: "测试.txt", DeclaredMediaType: "text/plain", DeclaredSizeBytes: 1}
}
func TestFileReservationIdempotency(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	s := policystore.Service{DB: c}
	ctx := context.Background()
	p := reservationParams()
	a, e := s.ReserveFile(ctx, publisher(), p)
	if e != nil || a.Duplicate || a.File.State != files.StateAllocated {
		t.Fatal(a, e)
	}
	b, e := s.ReserveFile(ctx, publisher(), p)
	if e != nil || !b.Duplicate || b.File.ID != a.File.ID || !b.File.UploadExpiresAt.Equal(a.File.UploadExpiresAt) {
		t.Fatal(b, e)
	}
	p.OriginalFilename = "different.txt"
	if _, e = s.ReserveFile(ctx, publisher(), p); !errors.Is(e, files.ErrUploadConflict) {
		t.Fatal(e)
	}
	var f, ev, au, seq, ob int
	e = c.QueryRow(ctx, `SELECT (SELECT count(*) FROM file_objects),(SELECT count(*) FROM file_lifecycle_events),(SELECT count(*) FROM audit_events WHERE action='file_reserve'),(SELECT last_seq FROM conversations WHERE id=$1),(SELECT count(*) FROM outbox_events)`, directA).Scan(&f, &ev, &au, &seq, &ob)
	if e != nil || f != 1 || ev != 1 || au != 1 || seq != 0 || ob != 0 {
		t.Fatal(f, ev, au, seq, ob, e)
	}
	run(t, c, "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1", tenantA)
	if _, e = s.ReserveFile(ctx, publisher(), reservationParams()); e != nil {
		t.Fatal("duplicate charged against disabled config", e)
	}
}
func TestFileReservationBudgetConcurrency(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	run(t, c, "UPDATE tenant_file_upload_policy SET tenant_storage_budget_bytes=26214400 WHERE tenant_id=$1", tenantA)
	peer := filePeer(t, c)
	ctx := context.Background()
	p := reservationParams()
	p.DeclaredSizeBytes = 26214400
	ch := make(chan error, 2)
	go func() { _, e := (policystore.Service{DB: c}).ReserveFile(ctx, publisher(), p); ch <- e }()
	p2 := p
	p2.UploadRequestID = clientB
	go func() { _, e := (policystore.Service{DB: peer}).ReserveFile(ctx, publisher(), p2); ch <- e }()
	var ok, denied int
	for i := 0; i < 2; i++ {
		e := <-ch
		if e == nil {
			ok++
		} else if errors.Is(e, files.ErrStorageBudgetExceeded) {
			denied++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || denied != 1 {
		t.Fatal(ok, denied)
	}
}
func TestFileReservationAtomicEvidence(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	run(t, c, `CREATE FUNCTION refuse_file_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER refuse_file_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_file_audit()`)
	if _, e := (policystore.Service{DB: c}).ReserveFile(context.Background(), publisher(), reservationParams()); e == nil {
		t.Fatal("audit failure accepted")
	}
	var n int
	if e := c.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM file_objects)+(SELECT count(*) FROM file_lifecycle_events)").Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestFileMetadataOwnership(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	s := policystore.Service{DB: c}
	ctx := context.Background()
	r, e := s.ReserveFile(ctx, publisher(), reservationParams())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetOwnFile(ctx, groupMemberIdentity(), r.File.ID); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal("foreign", e)
	}
	run(t, c, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", directA)
	if _, e = s.GetOwnFile(ctx, publisher(), r.File.ID); e != nil {
		t.Fatal("blocked owner status", e)
	}
	if _, e = s.ReserveFile(ctx, publisher(), reservationParams()); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal("blocked send", e)
	}
	run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp() WHERE id=$1", adminM)
	if _, e = s.GetOwnFile(ctx, publisher(), r.File.ID); !errors.Is(e, files.ErrInvalidIdentity) {
		t.Fatal("expired identity", e)
	}
}
func TestFileReservationWaitAuthorization(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	peer := filePeer(t, c)
	ctx := context.Background()
	run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp()+interval '2 seconds' WHERE id=$1", adminM)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT tenant_id FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE", tenantA); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() {
		_, e := (policystore.Service{DB: peer}).ReserveFile(ctx, publisher(), reservationParams())
		ch <- e
	}()
	waitFileLock(t, c, peer, "", func() { tx.Rollback(ctx) })
	time.Sleep(2300 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrInvalidIdentity) {
		t.Fatal(e)
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM file_objects").Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
