// Package filedownload manages bounded downloads; tickets are internal evidence, not access grants.
package filedownload

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"regexp"
	"time"
)

var (
	ErrNotFound        = errors.New("download unavailable")
	ErrInvalidIdentity = errors.New("invalid download identity")
	ErrBusy            = errors.New("download in progress")
	ErrAuditPending    = errors.New("download audit pending")
	ErrLimit           = errors.New("download limit reached")
	ErrUnavailable     = errors.New("download dependency unavailable")
)

type Ticket struct {
	SessionID, OwnerID, LeaseToken string
	Identity                       access.TrustedIdentity
	File                           files.Metadata
	MessageID                      string
	MessageSeq                     int64
	Deadline, LeaseExpiresAt       time.Time
}
type Result struct {
	Outcome, Reason string
	BytesWritten    int64
}
type Repository interface {
	BeginFileDownload(context.Context, access.TrustedIdentity, string, string, time.Time) (Ticket, error)
	AuthorizeFileDownload(context.Context, access.TrustedIdentity, Ticket) error
	CheckFileDownload(context.Context, access.TrustedIdentity, Ticket) error
	FinishFileDownload(context.Context, Ticket, Result) error
}

var canonicalID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func finite(t time.Time) bool { return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 }
func ValidateTicket(t Ticket) error {
	for _, id := range []string{t.SessionID, t.OwnerID, t.LeaseToken, t.MessageID, t.Identity.TenantID, t.Identity.UserID, t.Identity.ActingMembershipID} {
		if !canonicalID.MatchString(id) {
			return ErrUnavailable
		}
	}
	if files.ValidateMetadata(t.File) != nil || t.File.State != files.StateReady || t.Identity.TenantID != t.File.TenantID || t.MessageSeq < 1 || !finite(t.Deadline) || !finite(t.LeaseExpiresAt) || !t.LeaseExpiresAt.After(t.File.UpdatedAt) || t.LeaseExpiresAt.After(t.Deadline) {
		return ErrUnavailable
	}
	return nil
}

// BytesWritten is a response-writer observation, never client receipt; unknown is not inferred complete.
func ValidateResult(expectedSize int64, r Result) error {
	if expectedSize < 1 || expectedSize > files.MaxFileSizeBytes || r.BytesWritten < 0 || r.BytesWritten > expectedSize {
		return ErrUnavailable
	}
	switch r.Outcome {
	case "completed":
		if r.Reason == "completed" && r.BytesWritten == expectedSize {
			return nil
		}
	case "interrupted":
		switch r.Reason {
		case "client_disconnected", "token_expired", "authorization_revoked", "logical_expiry", "cleanup_pending", "dependency_unavailable", "integrity_mismatch", "timeout", "audit_unavailable":
			return nil
		}
	case "unknown":
		if r.Reason == "process_lost" || r.Reason == "unknown_result" {
			return nil
		}
	}
	return ErrUnavailable
}
