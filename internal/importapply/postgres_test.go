package importapply

import (
	"bytes"
	"context"
	"errors"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
)

func TestAppendPGApplyAtomic(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, e := NewService(f.Pool, f.Schema)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	raw := applyInput(t)
	report, doc := p.EvaluateDocument(ctx, raw)
	if doc == nil {
		t.Fatalf("fixture invalid: %+v", report.Issues)
	}
	if e = s.CheckReady(ctx); e != nil {
		t.Fatal(e)
	}
	r, e := s.Apply(ctx, actor, fixtureRequest, raw)
	if e != nil || r.Receipt.State != Applied || r.Receipt.Counts["total"].Inserted != 6 || r.Replay {
		t.Fatal("atomic six-table insert", e)
	}
	count := databaseCounts(t, f)
	if count != [8]int{2, 2, 1, 2, 2, 1, 1, 1} {
		t.Fatal("atomic counts", count)
	}
	same, e := s.Apply(ctx, actor, "97000000-0000-4000-8000-000000000001", raw)
	if e != nil || same.Receipt.Counts["total"].Inserted != 0 || same.Receipt.Counts["total"].Identical != 7 {
		t.Fatal("identical reimport", e)
	}
	changed := bytes.Replace(raw, []byte("New person"), []byte("Changed person"), 1)
	rejected, e := s.Apply(ctx, actor, "97000000-0000-4000-8000-000000000002", changed)
	if e != nil || rejected.Receipt.State != Rejected || rejected.Receipt.Counts["total"].Inserted != 0 || rejected.Receipt.Counts["users"].Conflict != 1 {
		t.Fatal("difference did not reject batch", e)
	}
}
func TestAppendPGReplayBinding(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, _ := NewService(f.Pool, f.Schema)
	ctx := context.Background()
	raw := applyInput(t)
	first, e := s.Apply(ctx, actor, fixtureRequest, raw)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.Apply(ctx, actor, fixtureRequest, raw)
	if e != nil || !replay.Replay || !bytes.Equal(first.Encoded, replay.Encoded) {
		t.Fatal("not exact replay", e)
	}
	_, e = s.Apply(ctx, actor, fixtureRequest, append(raw, ' '))
	if !errors.Is(e, ErrKeyConflict) {
		t.Fatal("same-key different bytes accepted", e)
	}
	get, e := s.Get(ctx, actor, fixtureRequest)
	if e != nil || !bytes.Equal(first.Encoded, get.Encoded) {
		t.Fatal("query receipt", e)
	}
	other := actor
	other.Identity.UserID = "98000000-0000-4000-8000-000000000001"
	other.Identity.ActingMembershipID = "98000000-0000-4000-8000-000000000002"
	other.Subject = "other"
	for _, sql := range []string{"INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('98000000-0000-4000-8000-000000000001',$1,'other','Other')", "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES('98000000-0000-4000-8000-000000000002',$1,'98000000-0000-4000-8000-000000000001','95000000-0000-4000-8000-000000000011','2020-01-01Z')", "INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES('98000000-0000-4000-8000-000000000003',$1,'98000000-0000-4000-8000-000000000002','95000000-0000-4000-8000-000000000011','group_admin','2020-01-01Z')", "INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES('https://sso.test','other',$1,'98000000-0000-4000-8000-000000000001')"} {
		if _, e = f.Admin.Exec(ctx, sql, fixtureTenant); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Get(ctx, other, fixtureRequest); e != nil {
		t.Fatal("current group admin cannot query", e)
	}
	if _, e = s.Apply(ctx, other, fixtureRequest, raw); !errors.Is(e, ErrKeyConflict) {
		t.Fatal("actor binding ignored", e)
	}
	if c := databaseCounts(t, f); c[6] != 1 || c[7] != 1 {
		t.Fatal("replay duplicated terminal/audit")
	}
}
func TestAppendPGSavepointReject(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, _ := NewService(f.Pool, f.Schema)
	ctx := context.Background()
	before := databaseCounts(t, f)
	if _, e := f.Admin.Exec(ctx, "ALTER TABLE users ADD CHECK(global_employee_no<>'newperson')"); e != nil {
		t.Fatal(e)
	}
	result, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t))
	if e != nil || result.Receipt.State != Rejected || result.Receipt.Reason != DatabaseConstraintConflict || result.Receipt.Counts["total"].Inserted != 0 {
		t.Fatal("constraint failure not rejected atomically", e)
	}
	after := databaseCounts(t, f)
	before[6]++
	before[7]++
	if before != after {
		t.Fatal("savepoint leaked earlier tables", after)
	}
}
func TestAppendPGAuditFailure(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, _ := NewService(f.Pool, f.Schema)
	ctx := context.Background()
	before := databaseCounts(t, f)
	if _, e := f.Admin.Exec(ctx, "ALTER TABLE audit_events ADD CHECK(action<>'controlled_import.apply')"); e != nil {
		t.Fatal(e)
	}
	_, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t))
	if !errors.Is(e, ErrAuditUnavailable) {
		t.Fatal("audit failure mislabeled", e)
	}
	after := databaseCounts(t, f)
	if before != after {
		t.Fatal("audit failure partially committed", after)
	}
}
func TestAppendPGLockCleanup(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, _ := NewService(f.Pool, f.Schema)
	ctx := context.Background()
	if _, e := f.Admin.Exec(ctx, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_advisory_unlock(bigint) FROM PUBLIC"); e != nil {
		t.Fatal(e)
	}
	defer f.Admin.Exec(ctx, "GRANT EXECUTE ON FUNCTION pg_catalog.pg_advisory_unlock(bigint) TO PUBLIC")
	if _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); e != nil {
		t.Fatal("committed receipt lost due cleanup error", e)
	}
	var held int
	if e := f.Admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype='advisory' AND pid IN (SELECT pid FROM pg_catalog.pg_stat_activity WHERE usename='im_import_writer')").Scan(&held); e != nil || held != 0 {
		t.Fatal("session lock retained", e, held)
	}
	if f.Pool.Stat().TotalConns() != 0 {
		t.Fatal("unclean connection returned to pool")
	}
	// Full UUIDs remain receipt lookup keys, regardless of lock key collisions.
	tx, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	_, found, e := lookupReceipt(ctx, tx, f.Schema, fixtureTenant, "97000000-0000-4000-8000-000000000999")
	if e != nil || found {
		t.Fatal("wrong batch found")
	}
}
