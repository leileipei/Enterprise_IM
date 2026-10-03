package files

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

var fileTime = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

func cloneFile(m Metadata) Metadata {
	m.RequestDigest = bytes.Clone(m.RequestDigest)
	m.SHA256 = bytes.Clone(m.SHA256)
	m.ScanSHA256 = bytes.Clone(m.ScanSHA256)
	if m.ActualSizeBytes != nil {
		x := *m.ActualSizeBytes
		m.ActualSizeBytes = &x
	}
	for _, pp := range []**time.Time{&m.UploadedAt, &m.ScannedAt, &m.DeletionRequestedAt, &m.DeletedAt} {
		if *pp != nil {
			x := **pp
			*pp = &x
		}
	}
	return m
}
func allocatedFile() Metadata {
	p := validCreateFixture()
	d, _ := CreationDigest(p)
	return Metadata{CreateParams: p, ID: "10000000-0000-4000-8000-00000000000f", RequestDigest: d[:], State: StateAllocated, CreatedAt: fileTime, UpdatedAt: fileTime, UploadExpiresAt: fileTime.Add(time.Hour)}
}
func advanceFile(m Metadata, to State) Metadata {
	n := cloneFile(m)
	n.State = to
	n.StateVersion++
	n.UpdatedAt = n.UpdatedAt.Add(time.Second)
	switch to {
	case StateUploaded:
		n.ObjectKey = "tenants/" + n.TenantID + "/files/" + n.ID
		n.ObjectVersionID = "opaque-V1"
		size := n.DeclaredSizeBytes
		n.ActualSizeBytes = &size
		n.SHA256 = bytes.Repeat([]byte{0xab}, 32)
		n.DetectedMediaType = "application/pdf"
		at := n.UpdatedAt
		n.UploadedAt = &at
	case StateScanning:
		n.ScanJobID = "10000000-0000-4000-8000-000000000061"
		if m.State == StateScanFailed {
			n.ScanJobID = "10000000-0000-4000-8000-000000000062"
		}
		n.ScanEngine = ""
		n.ScanDefinitionVersion = ""
		n.ScannedAt = nil
		n.ScanSHA256 = nil
	case StateReady, StateRejected:
		n.ScanEngine = "engine"
		n.ScanDefinitionVersion = "definitions-1"
		at := n.UpdatedAt
		n.ScannedAt = &at
		n.ScanSHA256 = bytes.Clone(n.SHA256)
	case StateScanFailed:
		n.ScanEngine = ""
		n.ScanDefinitionVersion = ""
		n.ScannedAt = nil
		n.ScanSHA256 = nil
	case StateDeletePending:
		at := n.UpdatedAt
		n.DeletionRequestedAt = &at
	case StateDeleted:
		at := n.UpdatedAt
		n.DeletedAt = &at
		n.OriginalFilename = ""
		n.DeclaredMediaType = ""
		n.ObjectKey = ""
		n.ObjectVersionID = ""
		n.DetectedMediaType = ""
		n.SHA256 = nil
		n.ScanJobID = ""
		n.ScanEngine = ""
		n.ScanDefinitionVersion = ""
		n.ScannedAt = nil
		n.ScanSHA256 = nil
	}
	return n
}
func fileAt(s State) Metadata {
	m := allocatedFile()
	if s == StateAllocated {
		return m
	}
	m = advanceFile(m, StateUploaded)
	if s == StateUploaded {
		return m
	}
	m = advanceFile(m, StateScanning)
	if s == StateScanning {
		return m
	}
	if s == StateReady || s == StateRejected || s == StateScanFailed {
		return advanceFile(m, s)
	}
	m = advanceFile(m, StateReady)
	m = advanceFile(m, StateDeletePending)
	if s == StateDeletePending {
		return m
	}
	return advanceFile(m, StateDeleted)
}
func TestFileMetadataStageCompleteness(t *testing.T) {
	for _, s := range fileStates {
		if err := ValidateMetadata(fileAt(s)); err != nil {
			t.Fatalf("valid %s: %v", s, err)
		}
	}
	cases := []struct {
		name  string
		state State
		mut   func(*Metadata)
	}{
		{"allocated empty bytea", StateAllocated, func(m *Metadata) { m.SHA256 = []byte{} }},
		{"allocated empty scan bytea", StateAllocated, func(m *Metadata) { m.ScanSHA256 = []byte{} }},
		{"uploaded empty scan bytea", StateUploaded, func(m *Metadata) { m.ScanSHA256 = []byte{} }},
		{"deleted empty bytea", StateDeleted, func(m *Metadata) { m.SHA256 = []byte{} }},
		{"allocated content", StateAllocated, func(m *Metadata) { m.ObjectKey = "unsealed" }},
		{"allocated job", StateAllocated, func(m *Metadata) { m.ScanJobID = "10000000-0000-4000-8000-000000000061" }},
		{"missing key", StateUploaded, func(m *Metadata) { m.ObjectKey = "" }},
		{"foreign key path", StateUploaded, func(m *Metadata) { m.ObjectKey = "tenants/other/files/other" }},
		{"missing size", StateUploaded, func(m *Metadata) { m.ActualSizeBytes = nil }},
		{"wrong size", StateUploaded, func(m *Metadata) { *m.ActualSizeBytes = 2 }},
		{"short sha", StateUploaded, func(m *Metadata) { m.SHA256 = m.SHA256[:31] }},
		{"long sha", StateUploaded, func(m *Metadata) { m.SHA256 = append(m.SHA256, 0) }},
		{"missing version", StateUploaded, func(m *Metadata) { m.ObjectVersionID = "" }},
		{"control version", StateUploaded, func(m *Metadata) { m.ObjectVersionID = "a\u009fb" }},
		{"upload expired", StateUploaded, func(m *Metadata) { m.UploadExpiresAt = *m.UploadedAt }},
		{"zero created", StateAllocated, func(m *Metadata) { m.CreatedAt = time.Time{} }},
		{"zero updated", StateAllocated, func(m *Metadata) { m.UpdatedAt = time.Time{} }},
		{"past updated", StateAllocated, func(m *Metadata) { m.UpdatedAt = m.CreatedAt.Add(-time.Second) }},
		{"equal expiry", StateAllocated, func(m *Metadata) { m.UploadExpiresAt = m.CreatedAt }},
		{"negative version", StateAllocated, func(m *Metadata) { m.StateVersion = -1 }},
		{"allocated nonzero", StateAllocated, func(m *Metadata) { m.StateVersion = 1 }},
		{"future upload", StateUploaded, func(m *Metadata) { at := m.UpdatedAt.Add(time.Second); m.UploadedAt = &at }},
		{"missing scan job", StateScanning, func(m *Metadata) { m.ScanJobID = "" }},
		{"scanning result", StateScanning, func(m *Metadata) { m.ScanSHA256 = bytes.Clone(m.SHA256) }},
		{"missing engine", StateReady, func(m *Metadata) { m.ScanEngine = "" }},
		{"missing definitions", StateReady, func(m *Metadata) { m.ScanDefinitionVersion = "" }},
		{"missing scanned time", StateReady, func(m *Metadata) { m.ScannedAt = nil }},
		{"scan before upload", StateReady, func(m *Metadata) { at := m.UploadedAt.Add(-time.Second); m.ScannedAt = &at }},
		{"wrong scan sha", StateReady, func(m *Metadata) { m.ScanSHA256[0]++ }},
		{"failed clean result", StateScanFailed, func(m *Metadata) { m.ScanSHA256 = bytes.Clone(m.SHA256) }},
		{"missing deletion time", StateDeletePending, func(m *Metadata) { m.DeletionRequestedAt = nil }},
		{"missing deleted time", StateDeleted, func(m *Metadata) { m.DeletedAt = nil }},
		{"deleted incomplete seal", StateDeleted, func(m *Metadata) { m.ObjectKey = "restore" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fileAt(tc.state)
			tc.mut(&m)
			if e := ValidateMetadata(m); !errors.Is(e, ErrInvalidMetadata) {
				t.Fatal("invalid snapshot accepted", e)
			}
		})
	}
}
