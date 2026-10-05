package policystore_test

import (
	"bytes"
	"context"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"testing"
	"time"
)

var sqlFileStates = []string{"allocated", "uploaded", "scanning", "ready", "rejected", "scan_failed", "delete_pending", "deleted"}
var sqlFileEdges = map[[2]string]bool{{"allocated", "uploaded"}: true, {"uploaded", "scanning"}: true, {"scanning", "ready"}: true, {"scanning", "rejected"}: true, {"scanning", "scan_failed"}: true, {"scan_failed", "scanning"}: true, {"allocated", "delete_pending"}: true, {"uploaded", "delete_pending"}: true, {"scanning", "delete_pending"}: true, {"ready", "delete_pending"}: true, {"rejected", "delete_pending"}: true, {"scan_failed", "delete_pending"}: true, {"delete_pending", "deleted"}: true}

func TestFileFoundationSchemaLifecycle(t *testing.T) {
	c := fileFoundationDB(t)
	seedDirectConversation(t, c)
	for _, from := range sqlFileStates {
		for _, to := range sqlFileStates {
			t.Run(from+"_"+to, func(t *testing.T) {
				m := storedFile(t, c, from)
				e := writeFile(c, fileNext(m, to), false)
				if sqlFileEdges[[2]string{from, to}] {
					if e != nil {
						t.Fatal(e)
					}
				} else {
					expectFileSQLState(t, e, "23514")
				}
			})
		}
	}
	m := freshFile()
	m.State = "ready"
	expectFileSQLState(t, writeFile(c, m, true), "23514")
	m = freshFile()
	m.StateVersion = 1
	expectFileSQLState(t, writeFile(c, m, true), "23514")
	m = storedFile(t, c, "scanning")
	for _, tc := range []struct {
		name string
		mut  func(*files.Metadata)
	}{
		{"version unchanged", func(n *files.Metadata) { n.StateVersion = m.StateVersion }}, {"version jump", func(n *files.Metadata) { n.StateVersion += 1 }}, {"wrong job", func(n *files.Metadata) { n.ScanJobID = "10000000-0000-4000-8000-000000000069" }}, {"object version", func(n *files.Metadata) { n.ObjectVersionID = "other" }}, {"object path", func(n *files.Metadata) { n.ObjectKey = "tenants/foreign/files/foreign" }}, {"content replacement", func(n *files.Metadata) { n.SHA256 = bytes.Repeat([]byte{3}, 32); n.ScanSHA256 = bytes.Clone(n.SHA256) }}, {"short sha", func(n *files.Metadata) { n.SHA256 = []byte{1} }}, {"long sha", func(n *files.Metadata) { n.SHA256 = bytes.Repeat([]byte{1}, 33) }}, {"size", func(n *files.Metadata) { s := int64(2); n.ActualSizeBytes = &s }}, {"scan time", func(n *files.Metadata) { a := m.UploadedAt.Add(-time.Second); n.ScannedAt = &a }}, {"expiry", func(n *files.Metadata) { n.UploadExpiresAt = *m.UploadedAt }}, {"created", func(n *files.Metadata) { n.CreatedAt = n.CreatedAt.Add(-time.Second) }}, {"backdated", func(n *files.Metadata) { n.UpdatedAt = m.UpdatedAt.Add(-time.Second) }}, {"null scan hash", func(n *files.Metadata) { n.ScanSHA256 = nil }}, {"missing engine", func(n *files.Metadata) { n.ScanEngine = "" }}, {"bad source", func(n *files.Metadata) { n.UploaderMembershipID = targetM2 }}, {"filename", func(n *files.Metadata) { n.OriginalFilename = "改名.pdf" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := fileNext(m, "ready")
			tc.mut(&n)
			expectFileSQLState(t, writeFile(c, n, false), "23514")
		})
	}
	if e := writeFile(c, m, false); e != nil {
		t.Fatal("no-op", e)
	}
	_, e := c.Exec(context.Background(), "DELETE FROM file_objects WHERE id=$1", m.ID)
	expectFileSQLState(t, e, "23514")
	failed := storedFile(t, c, "scan_failed")
	retry := fileNext(failed, "scanning")
	retry.ScanJobID = failed.ScanJobID
	expectFileSQLState(t, writeFile(c, retry, false), "23514")
}
func TestFileFoundationSchemaDeletedCannotRestore(t *testing.T) {
	c := fileFoundationDB(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, "deleted")
	for _, set := range []string{"state='ready'", "original_filename='restore.pdf'", "sha256=decode(repeat('ab',32),'hex')", "object_key='restore'"} {
		_, e := c.Exec(context.Background(), "UPDATE file_objects SET "+set+" WHERE id=$1", m.ID)
		expectFileSQLState(t, e, "23514")
	}
}
func TestFileFoundationSchemaEventEvidence(t *testing.T) {
	c := fileFoundationDB(t)
	seedDirectConversation(t, c)
	m := storedFile(t, c, "allocated")
	q := `INSERT INTO file_lifecycle_events(tenant_id,file_id,state_version,from_state,to_state,reason_code,occurred_at,actor_kind,actor_user_id,acting_membership_id,worker_job_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	initial := []any{tenantA, m.ID, int64(0), nil, "allocated", "allocated", at, "user", adminA, adminM, nil}
	run(t, c, q, initial...)
	expectFileSQLState(t, func() error { _, e := c.Exec(context.Background(), q, initial...); return e }(), "23505")
	// Advancing the file must not invalidate its historical event.
	n := fileNext(m, "uploaded")
	if e := writeFile(c, n, false); e != nil {
		t.Fatal(e)
	}
	run(t, c, q, tenantA, m.ID, int64(1), "allocated", "uploaded", "upload_sealed", at.Add(time.Second), "user", adminA, adminM, nil)
	run(t, c, q, tenantA, m.ID, int64(2), "uploaded", "scanning", "scan_started", at.Add(2*time.Second), "worker", nil, nil, "10000000-0000-4000-8000-000000000061")
	run(t, c, q, tenantA, m.ID, int64(3), "scanning", "delete_pending", "deletion_requested", at.Add(3*time.Second), "worker", nil, nil, "10000000-0000-4000-8000-000000000062")
	for _, tc := range []struct {
		i    int
		v    any
		code string
	}{{0, tenantB, "23503"}, {9, targetM, "23503"}, {4, "bogus", "23514"}, {3, "unknown", "23514"}, {5, "unknown", "23514"}, {5, "scan_clean", "23514"}, {2, int64(-1), "23514"}, {2, int64(1), "23514"}, {7, "worker", "23514"}, {10, "10000000-0000-4000-8000-000000000061", "23514"}} {
		isolated := storedFile(t, c, "allocated")
		a := append([]any(nil), initial...)
		a[1] = isolated.ID
		a[tc.i] = tc.v
		_, e := c.Exec(context.Background(), q, a...)
		expectFileSQLState(t, e, tc.code)
	}
	for _, sql := range []string{"UPDATE file_lifecycle_events SET reason_code='changed' WHERE file_id=$1", "DELETE FROM file_lifecycle_events WHERE file_id=$1"} {
		_, e := c.Exec(context.Background(), sql, m.ID)
		expectFileSQLState(t, e, "23514")
	}
}
