package files

import (
	"errors"
	"time"
)

var ErrInvalidEvent = errors.New("invalid file event")

type Event struct {
	TenantID, FileID                                        string
	StateVersion                                            int64
	FromState, ToState                                      State
	Reason                                                  Reason
	OccurredAt                                              time.Time
	ActorKind, ActorUserID, ActingMembershipID, WorkerJobID string
}

// ValidateEvent checks evidence shape, not an authenticated actor or atomic persistence.
func ValidateEvent(e Event) error {
	r, err := TransitionReason(e.FromState, e.ToState)
	if err != nil || r != e.Reason || !validUUID(e.TenantID) || !validUUID(e.FileID) || e.OccurredAt.IsZero() || (e.FromState == "" && e.StateVersion != 0) || (e.FromState != "" && e.StateVersion <= 0) {
		return ErrInvalidEvent
	}
	switch e.ActorKind {
	case "user":
		if !validUUID(e.ActorUserID) || !validUUID(e.ActingMembershipID) || e.WorkerJobID != "" {
			return ErrInvalidEvent
		}
	case "worker":
		if e.ActorUserID != "" || e.ActingMembershipID != "" || !validUUID(e.WorkerJobID) {
			return ErrInvalidEvent
		}
	default:
		return ErrInvalidEvent
	}
	switch r {
	case ReasonAllocated, ReasonUploadSealed:
		if e.ActorKind != "user" {
			return ErrInvalidEvent
		}
	case ReasonScanStarted, ReasonScanClean, ReasonScanRejected, ReasonScanError, ReasonScanRetry:
		if e.ActorKind != "worker" {
			return ErrInvalidEvent
		}
	}
	return nil
}
