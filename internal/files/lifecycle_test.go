package files

import (
	"errors"
	"math"
	"testing"
	"time"
)

var fileStates = []State{StateAllocated, StateUploaded, StateScanning, StateReady, StateRejected, StateScanFailed, StateDeletePending, StateDeleted}
var allowedFileEdges = map[[2]State]Reason{
	{StateAllocated, StateUploaded}: "upload_sealed", {StateUploaded, StateScanning}: "scan_started", {StateScanning, StateReady}: "scan_clean", {StateScanning, StateRejected}: "scan_rejected", {StateScanning, StateScanFailed}: "scan_error", {StateScanFailed, StateScanning}: "scan_retry", {StateAllocated, StateDeletePending}: "deletion_requested", {StateUploaded, StateDeletePending}: "deletion_requested", {StateScanning, StateDeletePending}: "deletion_requested", {StateReady, StateDeletePending}: "deletion_requested", {StateRejected, StateDeletePending}: "deletion_requested", {StateScanFailed, StateDeletePending}: "deletion_requested", {StateDeletePending, StateDeleted}: "object_deleted",
}

func TestFileLifecycleAllowedAndDeniedEdges(t *testing.T) {
	for _, from := range fileStates {
		for _, to := range fileStates {
			t.Run(string(from)+"_"+string(to), func(t *testing.T) {
				want, allowed := allowedFileEdges[[2]State{from, to}]
				reason, e := TransitionReason(from, to)
				b := fileAt(from)
				n := advanceFile(b, to)
				err := ValidateTransition(b, n)
				if allowed {
					if e != nil || reason != want || err != nil {
						t.Fatalf("valid edge reason=%s err=%v transition=%v", reason, e, err)
					}
				} else if !errors.Is(e, ErrInvalidTransition) || !errors.Is(err, ErrInvalidTransition) {
					t.Fatal("forbidden edge accepted", reason, e, err)
				}
			})
		}
	}
	if r, e := TransitionReason("", StateAllocated); e != nil || r != "allocated" {
		t.Fatal("creation event edge missing")
	}
	for _, pair := range [][2]State{{"bogus", StateReady}, {StateAllocated, "bogus"}, {"", StateReady}} {
		if _, e := TransitionReason(pair[0], pair[1]); !errors.Is(e, ErrInvalidTransition) {
			t.Fatal("unknown edge accepted")
		}
	}
}
func TestFileTransitionImmutableSealedContent(t *testing.T) {
	b := fileAt(StateScanning)
	for _, tc := range []struct {
		name string
		mut  func(*Metadata)
	}{
		{"tenant", func(m *Metadata) { m.TenantID = "10000000-0000-4000-8000-000000000001" }},
		{"conv", func(m *Metadata) { m.ConversationID = "10000000-0000-4000-8000-000000000001" }},
		{"uploader", func(m *Metadata) { m.UploaderUserID = "10000000-0000-4000-8000-000000000001" }},
		{"membership", func(m *Metadata) { m.UploaderMembershipID = "10000000-0000-4000-8000-000000000001" }},
		{"request", func(m *Metadata) { m.UploadRequestID = "10000000-0000-4000-8000-000000000001" }},
		{"request digest", func(m *Metadata) { m.RequestDigest[0]++ }},
		{"name", func(m *Metadata) { m.OriginalFilename = "改名.pdf" }},
		{"MIME", func(m *Metadata) { m.DeclaredMediaType = "image/png" }},
		{"size", func(m *Metadata) { m.DeclaredSizeBytes = 2; *m.ActualSizeBytes = 2 }},
		{"sha", func(m *Metadata) { m.SHA256[0]++; m.ScanSHA256[0]++ }},
		{"version", func(m *Metadata) { m.ObjectVersionID = "opaque-V2" }},
		{"detected type", func(m *Metadata) { m.DetectedMediaType = "image/png" }},
		{"created", func(m *Metadata) { m.CreatedAt = m.CreatedAt.Add(-time.Second) }},
		{"upload expiry", func(m *Metadata) { m.UploadExpiresAt = m.UploadExpiresAt.Add(time.Hour) }},
		{"upload time", func(m *Metadata) { at := m.UploadedAt.Add(time.Microsecond); m.UploadedAt = &at }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := advanceFile(b, StateReady)
			tc.mut(&n)
			if e := ValidateTransition(b, n); !errors.Is(e, ErrInvalidTransition) {
				t.Fatal("changed sealed source accepted", e)
			}
		})
	}
}
func TestFileTransitionRejectsStaleScanAndVersion(t *testing.T) {
	b := fileAt(StateScanning)
	for _, mut := range []func(*Metadata){func(m *Metadata) { m.ScanJobID = "10000000-0000-4000-8000-000000000062" }, func(m *Metadata) { m.StateVersion = b.StateVersion }, func(m *Metadata) { m.StateVersion = b.StateVersion + 2 }, func(m *Metadata) { m.UpdatedAt = b.UpdatedAt.Add(-time.Second) }} {
		n := advanceFile(b, StateReady)
		mut(&n)
		if e := ValidateTransition(b, n); !errors.Is(e, ErrInvalidTransition) {
			t.Fatal("stale scan/version accepted")
		}
	}
	b.StateVersion = math.MaxInt64
	n := advanceFile(b, StateReady)
	if e := ValidateTransition(b, n); !errors.Is(e, ErrInvalidTransition) {
		t.Fatal("version overflow accepted")
	}
	failed := fileAt(StateScanFailed)
	retry := advanceFile(failed, StateScanning)
	if e := ValidateTransition(failed, retry); e != nil {
		t.Fatal("valid retry", e)
	}
	retry.ScanJobID = failed.ScanJobID
	if e := ValidateTransition(failed, retry); !errors.Is(e, ErrInvalidTransition) {
		t.Fatal("retry reused old job")
	}
}
func TestFileDeleteTerminalCannotRevive(t *testing.T) {
	for _, state := range []State{StateAllocated, StateReady} {
		b := fileAt(state)
		pending := advanceFile(b, StateDeletePending)
		deleted := advanceFile(pending, StateDeleted)
		if e := ValidateTransition(b, pending); e != nil {
			t.Fatal(e)
		}
		if e := ValidateTransition(pending, deleted); e != nil {
			t.Fatal(e)
		}
		for _, to := range []State{StateUploaded, StateReady, StateScanning, StateAllocated} {
			if e := ValidateTransition(pending, advanceFile(pending, to)); !errors.Is(e, ErrInvalidTransition) {
				t.Fatal("late response revived pending")
			}
			if e := ValidateTransition(deleted, advanceFile(deleted, to)); !errors.Is(e, ErrInvalidTransition) {
				t.Fatal("deleted revived")
			}
		}
		changed := cloneFile(deleted)
		changed.OriginalFilename = "restore.pdf"
		if e := ValidateTransition(deleted, changed); !errors.Is(e, ErrInvalidTransition) {
			t.Fatal("same-state metadata restored")
		}
	}
}
