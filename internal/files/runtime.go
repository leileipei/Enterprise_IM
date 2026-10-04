package files

import (
	"bytes"
	"errors"
	"time"
)

var (
	ErrUploadDisabled        = errors.New("file upload disabled")
	ErrFileNotFound          = errors.New("file not found")
	ErrInvalidIdentity       = errors.New("invalid file identity")
	ErrUploadConflict        = errors.New("file upload conflict")
	ErrAlreadyUploaded       = errors.New("file already uploaded")
	ErrUploadExpired         = errors.New("file upload expired")
	ErrStorageBudgetExceeded = errors.New("file storage budget exceeded")
	ErrUploadBusy            = errors.New("file upload busy")
	ErrRecoveryPending       = errors.New("file upload recovery pending")
	ErrFileTooLarge          = errors.New("file too large")
	ErrInvalidFileSize       = errors.New("invalid file size")
	ErrFileTypeNotAllowed    = errors.New("file type not allowed")
	ErrDependencyUnavailable = errors.New("file dependency unavailable")
	ErrLeaseLost             = errors.New("file lease lost")
)

type Measurement struct {
	SizeBytes         int64
	SHA256            [32]byte
	DetectedMediaType string
}
type UploadPhase string

const (
	UploadReceiving       UploadPhase = "receiving"
	UploadReceived        UploadPhase = "received"
	UploadStoring         UploadPhase = "storing"
	UploadRecoveryPending UploadPhase = "recovery_pending"
	UploadRecovered       UploadPhase = "recovered"
	UploadSealed          UploadPhase = "sealed"
	UploadReceiveFailed   UploadPhase = "receive_failed"
)

// UploadTicket is internal runtime evidence; it is never a public response.
type UploadTicket struct {
	File                           Metadata
	AttemptID, LeaseToken, OwnerID string
	LeaseExpiresAt                 time.Time
	Phase                          UploadPhase
	Measurement                    *Measurement
	ObjectVersionID                string
}
type RecoveryTicket struct {
	Upload                     UploadTicket
	JobID, LeaseToken, OwnerID string
	LeaseExpiresAt             time.Time
	LookupAttempt              int
}
type RecoveryEvidence struct {
	VersionID   string
	Measurement Measurement
	ReasonCode  string
	Resolved    bool
}
type ScanTicket struct {
	File                       Metadata
	JobID, LeaseToken, OwnerID string
	ClaimVersion               int64
	LeaseExpiresAt             time.Time
	Attempt                    int
}
type ScanDecision struct {
	State                                 State
	Engine, DefinitionVersion, ReasonCode string
	SHA256                                [32]byte
}
type Reservation struct {
	File      Metadata
	Duplicate bool
}

func ValidateMeasurement(m Measurement, f Metadata) error {
	if m.SizeBytes < 1 || m.SizeBytes > MaxFileSizeBytes || m.SizeBytes != f.DeclaredSizeBytes || (!supportedUploadType(m.DetectedMediaType) && m.DetectedMediaType != "application/octet-stream") {
		return ErrInvalidMetadata
	}
	return nil
}
func ValidateUploadTicket(t UploadTicket) error {
	if ValidateMetadata(t.File) != nil || !validUUID(t.AttemptID) || !validUUID(t.LeaseToken) || !validUUID(t.OwnerID) || !t.LeaseExpiresAt.After(t.File.CreatedAt) || t.LeaseExpiresAt.After(t.File.UploadExpiresAt) {
		return ErrInvalidMetadata
	}
	if t.Phase == UploadSealed {
		if t.File.State != StateUploaded {
			return ErrInvalidMetadata
		}
	} else if t.File.State != StateAllocated {
		return ErrInvalidMetadata
	}
	if t.ObjectVersionID != "" && (!opaque(t.ObjectVersionID) || t.ObjectVersionID == "null") {
		return ErrInvalidMetadata
	}
	switch t.Phase {
	case UploadReceiving, UploadReceiveFailed:
		if t.Measurement != nil || t.ObjectVersionID != "" {
			return ErrInvalidMetadata
		}
	case UploadReceived, UploadStoring, UploadRecoveryPending, UploadRecovered, UploadSealed:
		if t.Measurement == nil || ValidateMeasurement(*t.Measurement, t.File) != nil {
			return ErrInvalidMetadata
		}
		if t.Phase == UploadReceived && t.ObjectVersionID != "" {
			return ErrInvalidMetadata
		}
		if (t.Phase == UploadRecovered || t.Phase == UploadSealed) && t.ObjectVersionID == "" {
			return ErrInvalidMetadata
		}
		if t.Phase == UploadSealed && (t.ObjectVersionID != t.File.ObjectVersionID || t.Measurement.DetectedMediaType != t.File.DetectedMediaType || !bytes.Equal(t.Measurement.SHA256[:], t.File.SHA256)) {
			return ErrInvalidMetadata
		}
	default:
		return ErrInvalidMetadata
	}
	return nil
}
