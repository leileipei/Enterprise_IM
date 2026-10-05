package filedownload

import (
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"testing"
	"time"
)

func TestFileDownloadResultBounds(t *testing.T) {
	for _, r := range []Result{{Outcome: "completed", Reason: "completed", BytesWritten: 10}, {Outcome: "interrupted", Reason: "client_disconnected", BytesWritten: 4}, {Outcome: "unknown", Reason: "process_lost", BytesWritten: 0}} {
		if e := ValidateResult(10, r); e != nil {
			t.Fatal(r, e)
		}
	}
	for _, r := range []Result{{Outcome: "completed", Reason: "completed", BytesWritten: 9}, {Outcome: "completed", Reason: "timeout", BytesWritten: 10}, {Outcome: "completed", Reason: "completed", BytesWritten: 11}, {Outcome: "interrupted", Reason: "client_disconnected", BytesWritten: -1}, {Outcome: "unknown", Reason: "filename:secret", BytesWritten: 0}, {Outcome: "bogus", Reason: "completed", BytesWritten: 10}} {
		if e := ValidateResult(10, r); e == nil {
			t.Fatal("invalid fact accepted", r)
		}
	}
	for _, size := range []int64{0, -1, 26214401} {
		if e := ValidateResult(size, Result{Outcome: "completed", Reason: "completed", BytesWritten: size}); e == nil {
			t.Fatal("invalid object size accepted", size)
		}
	}
	if e := ValidateTicket(Ticket{}); e == nil {
		t.Fatal("empty download origin accepted")
	}
}

func TestFileDownloadTicketSources(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	created := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	uploaded := created.Add(time.Second)
	scanned := created.Add(2 * time.Second)
	size := int64(10)
	meta := files.Metadata{CreateParams: files.CreateParams{TenantID: id, ConversationID: id, UploaderUserID: id, UploaderMembershipID: id, UploadRequestID: id, OriginalFilename: "中文.txt", DeclaredMediaType: "text/plain", DeclaredSizeBytes: size}, ID: id, RequestDigest: make([]byte, 32), State: files.StateReady, StateVersion: 3, CreatedAt: created, UpdatedAt: scanned, UploadExpiresAt: created.Add(15 * time.Minute), ObjectKey: "tenants/" + id + "/files/" + id, ObjectVersionID: "fixed-v1", DetectedMediaType: "text/plain", ActualSizeBytes: &size, SHA256: make([]byte, 32), UploadedAt: &uploaded, ScanJobID: id, ScanEngine: "clamav", ScanDefinitionVersion: "fresh", ScannedAt: &scanned, ScanSHA256: make([]byte, 32)}
	good := Ticket{SessionID: id, OwnerID: id, LeaseToken: id, Identity: access.TrustedIdentity{TenantID: id, UserID: id, ActingMembershipID: id}, File: meta, MessageID: id, MessageSeq: 1, Deadline: created.Add(time.Minute), LeaseExpiresAt: created.Add(time.Minute)}
	if e := ValidateTicket(good); e != nil {
		t.Fatal("valid ready source denied", e)
	}
	for _, mut := range []func(*Ticket){func(t *Ticket) { t.Identity.TenantID = "00000000-0000-4000-8000-000000000002" }, func(t *Ticket) { t.MessageSeq = 0 }, func(t *Ticket) { t.LeaseToken = "bad" }, func(t *Ticket) { t.File.State = files.StateDeletePending }, func(t *Ticket) { t.LeaseExpiresAt = t.Deadline.Add(time.Second) }, func(t *Ticket) { t.Deadline = time.Time{} }} {
		bad := good
		mut(&bad)
		if e := ValidateTicket(bad); e == nil {
			t.Fatal("invalid source accepted", bad)
		}
	}
}
