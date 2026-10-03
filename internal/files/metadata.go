package files

import (
	"bytes"
	"time"
	"unicode"
	"unicode/utf8"
)

// Metadata describes evidence, never an authorization to access content.
type Metadata struct {
	CreateParams
	ID                                            string
	RequestDigest                                 []byte
	State                                         State
	StateVersion                                  int64
	CreatedAt, UpdatedAt, UploadExpiresAt         time.Time
	ObjectKey, ObjectVersionID, DetectedMediaType string
	ActualSizeBytes                               *int64
	SHA256                                        []byte
	UploadedAt                                    *time.Time
	ScanJobID, ScanEngine, ScanDefinitionVersion  string
	ScannedAt                                     *time.Time
	ScanSHA256                                    []byte
	DeletionRequestedAt, DeletedAt                *time.Time
}

func validUUID(s string) bool { return canonicalUUID(s) }
func opaque(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (m Metadata) measured() bool {
	return m.ActualSizeBytes != nil && *m.ActualSizeBytes == m.DeclaredSizeBytes && m.UploadedAt != nil && !m.UploadedAt.IsZero() && !m.UploadedAt.Before(m.CreatedAt) && m.UploadedAt.Before(m.UploadExpiresAt) && !m.UploadedAt.After(m.UpdatedAt)
}
func (m Metadata) sealEmpty() bool {
	return m.ActualSizeBytes == nil && m.UploadedAt == nil && m.ObjectKey == "" && m.ObjectVersionID == "" && m.DetectedMediaType == "" && m.SHA256 == nil
}
func (m Metadata) sealed() bool {
	return m.measured() && m.ObjectKey == "tenants/"+m.TenantID+"/files/"+m.ID && opaque(m.ObjectVersionID) && validMediaType(m.DetectedMediaType) && len(m.SHA256) == 32
}
func (m Metadata) scanEmpty() bool {
	return m.ScanJobID == "" && m.ScanEngine == "" && m.ScanDefinitionVersion == "" && m.ScannedAt == nil && m.ScanSHA256 == nil
}
func (m Metadata) scanPending() bool {
	return validUUID(m.ScanJobID) && m.ScanEngine == "" && m.ScanDefinitionVersion == "" && m.ScannedAt == nil && m.ScanSHA256 == nil
}
func (m Metadata) scanResult() bool {
	return validUUID(m.ScanJobID) && opaque(m.ScanEngine) && opaque(m.ScanDefinitionVersion) && m.ScannedAt != nil && !m.ScannedAt.IsZero() && m.UploadedAt != nil && !m.ScannedAt.Before(*m.UploadedAt) && !m.ScannedAt.After(m.UpdatedAt) && len(m.ScanSHA256) == 32 && bytes.Equal(m.ScanSHA256, m.SHA256)
}
func (m Metadata) contentCleared() bool {
	return m.OriginalFilename == "" && m.DeclaredMediaType == "" && m.ObjectKey == "" && m.ObjectVersionID == "" && m.DetectedMediaType == "" && m.SHA256 == nil && m.scanEmpty()
}
func (m Metadata) intact() bool {
	p, e := NormalizeCreate(m.CreateParams)
	return e == nil && p == m.CreateParams && ((m.sealEmpty() && m.scanEmpty()) || (m.sealed() && (m.scanEmpty() || m.scanPending() || m.scanResult())))
}
func (m Metadata) deletionTimeValid() bool {
	return m.DeletionRequestedAt != nil && !m.DeletionRequestedAt.IsZero() && !m.DeletionRequestedAt.Before(m.CreatedAt) && !m.DeletionRequestedAt.After(m.UpdatedAt) && (m.UploadedAt == nil || !m.DeletionRequestedAt.Before(*m.UploadedAt)) && (m.ScannedAt == nil || !m.DeletionRequestedAt.Before(*m.ScannedAt))
}

// ValidateMetadata checks a snapshot without asserting who wrote it or that a scanner ran.
func ValidateMetadata(m Metadata) error {
	for _, id := range []string{m.ID, m.TenantID, m.ConversationID, m.UploaderUserID, m.UploaderMembershipID, m.UploadRequestID} {
		if !validUUID(id) {
			return ErrInvalidMetadata
		}
	}
	if len(m.RequestDigest) != 32 || m.DeclaredSizeBytes < 1 || m.DeclaredSizeBytes > MaxFileSizeBytes || m.StateVersion < 0 || m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.UploadExpiresAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) || !m.UploadExpiresAt.After(m.CreatedAt) {
		return ErrInvalidMetadata
	}
	if m.State == StateDeleted {
		measured := (m.ActualSizeBytes == nil && m.UploadedAt == nil) || m.measured()
		if m.StateVersion > 0 && m.deletionTimeValid() && m.DeletedAt != nil && !m.DeletedAt.IsZero() && !m.DeletedAt.Before(*m.DeletionRequestedAt) && !m.DeletedAt.After(m.UpdatedAt) && (m.intact() || (m.contentCleared() && measured)) {
			return nil
		}
		return ErrInvalidMetadata
	}
	p, e := NormalizeCreate(m.CreateParams)
	if e != nil || p != m.CreateParams || m.DeletedAt != nil {
		return ErrInvalidMetadata
	}
	if m.State == StateDeletePending {
		if m.StateVersion > 0 && m.intact() && m.deletionTimeValid() {
			return nil
		}
		return ErrInvalidMetadata
	}
	if m.DeletionRequestedAt != nil {
		return ErrInvalidMetadata
	}
	switch m.State {
	case StateAllocated:
		if m.StateVersion == 0 && m.sealEmpty() && m.scanEmpty() {
			return nil
		}
	case StateUploaded:
		if m.StateVersion > 0 && m.sealed() && m.scanEmpty() {
			return nil
		}
	case StateScanning, StateScanFailed:
		if m.StateVersion > 0 && m.sealed() && m.scanPending() {
			return nil
		}
	case StateReady, StateRejected:
		if m.StateVersion > 0 && m.sealed() && m.scanResult() {
			return nil
		}
	}
	return ErrInvalidMetadata
}
