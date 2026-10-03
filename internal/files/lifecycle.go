package files

import (
	"bytes"
	"errors"
	"math"
	"time"
)

type State string

const (
	StateAllocated     State = "allocated"
	StateUploaded      State = "uploaded"
	StateScanning      State = "scanning"
	StateReady         State = "ready"
	StateRejected      State = "rejected"
	StateScanFailed    State = "scan_failed"
	StateDeletePending State = "delete_pending"
	StateDeleted       State = "deleted"
)

type Reason string

const (
	ReasonAllocated         Reason = "allocated"
	ReasonUploadSealed      Reason = "upload_sealed"
	ReasonScanStarted       Reason = "scan_started"
	ReasonScanClean         Reason = "scan_clean"
	ReasonScanRejected      Reason = "scan_rejected"
	ReasonScanError         Reason = "scan_error"
	ReasonScanRetry         Reason = "scan_retry"
	ReasonDeletionRequested Reason = "deletion_requested"
	ReasonObjectDeleted     Reason = "object_deleted"
)

var ErrInvalidTransition = errors.New("invalid file transition")

func TransitionReason(from, to State) (Reason, error) {
	switch {
	case from == "" && to == StateAllocated:
		return ReasonAllocated, nil
	case from == StateAllocated && to == StateUploaded:
		return ReasonUploadSealed, nil
	case from == StateUploaded && to == StateScanning:
		return ReasonScanStarted, nil
	case from == StateScanning && to == StateReady:
		return ReasonScanClean, nil
	case from == StateScanning && to == StateRejected:
		return ReasonScanRejected, nil
	case from == StateScanning && to == StateScanFailed:
		return ReasonScanError, nil
	case from == StateScanFailed && to == StateScanning:
		return ReasonScanRetry, nil
	case from == StateDeletePending && to == StateDeleted:
		return ReasonObjectDeleted, nil
	case to == StateDeletePending:
		switch from {
		case StateAllocated, StateUploaded, StateScanning, StateReady, StateRejected, StateScanFailed:
			return ReasonDeletionRequested, nil
		}
	}
	return "", ErrInvalidTransition
}
func equalTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}
func equalSize(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func sameScan(a, b Metadata) bool {
	return a.ScanJobID == b.ScanJobID && a.ScanEngine == b.ScanEngine && a.ScanDefinitionVersion == b.ScanDefinitionVersion && equalTime(a.ScannedAt, b.ScannedAt) && bytes.Equal(a.ScanSHA256, b.ScanSHA256)
}
func sameContent(a, b Metadata) bool {
	return a.OriginalFilename == b.OriginalFilename && a.DeclaredMediaType == b.DeclaredMediaType && a.ObjectKey == b.ObjectKey && a.ObjectVersionID == b.ObjectVersionID && a.DetectedMediaType == b.DetectedMediaType && bytes.Equal(a.SHA256, b.SHA256)
}

// ValidateTransition enforces an actual state change; persistence must also compare the expected version.
func ValidateTransition(b, n Metadata) error {
	if ValidateMetadata(b) != nil || ValidateMetadata(n) != nil {
		return ErrInvalidTransition
	}
	if _, e := TransitionReason(b.State, n.State); e != nil || b.State == "" {
		return ErrInvalidTransition
	}
	if b.StateVersion == math.MaxInt64 || n.StateVersion != b.StateVersion+1 || n.UpdatedAt.Before(b.UpdatedAt) {
		return ErrInvalidTransition
	}
	if b.ID != n.ID || b.TenantID != n.TenantID || b.ConversationID != n.ConversationID || b.UploaderUserID != n.UploaderUserID || b.UploaderMembershipID != n.UploaderMembershipID || b.UploadRequestID != n.UploadRequestID || !bytes.Equal(b.RequestDigest, n.RequestDigest) || b.DeclaredSizeBytes != n.DeclaredSizeBytes || !b.CreatedAt.Equal(n.CreatedAt) || !b.UploadExpiresAt.Equal(n.UploadExpiresAt) {
		return ErrInvalidTransition
	}
	clearing := n.State == StateDeleted && n.contentCleared()
	if !(b.State == StateAllocated && n.State == StateUploaded) {
		if !equalSize(b.ActualSizeBytes, n.ActualSizeBytes) || !equalTime(b.UploadedAt, n.UploadedAt) || (!clearing && !sameContent(b, n)) {
			return ErrInvalidTransition
		}
	} else if b.OriginalFilename != n.OriginalFilename || b.DeclaredMediaType != n.DeclaredMediaType {
		return ErrInvalidTransition
	}
	switch n.State {
	case StateScanning:
		if b.State == StateScanFailed && b.ScanJobID == n.ScanJobID {
			return ErrInvalidTransition
		}
	case StateReady, StateRejected, StateScanFailed:
		if b.ScanJobID != n.ScanJobID || (n.ScannedAt != nil && n.ScannedAt.Before(b.UpdatedAt)) {
			return ErrInvalidTransition
		}
	case StateDeletePending:
		if !sameScan(b, n) || n.DeletionRequestedAt.Before(b.UpdatedAt) {
			return ErrInvalidTransition
		}
	case StateDeleted:
		if !equalTime(b.DeletionRequestedAt, n.DeletionRequestedAt) || (!clearing && !sameScan(b, n)) || n.DeletedAt.Before(b.UpdatedAt) {
			return ErrInvalidTransition
		}
	}
	return nil
}
