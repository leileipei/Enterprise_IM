// Package filecleanup reconciles deletion commitments. Lease expiry is not absence evidence.
package filecleanup

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrLeaseLost  = errors.New("file cleanup lease lost")
	ErrBlocked    = errors.New("file cleanup blocked")
	ErrIncomplete = errors.New("file cleanup evidence incomplete")
)

type Ticket struct {
	JobID, TenantID, FileID, ConversationID, OwnerID, LeaseToken string
	PolicyVersion, StateVersion                                  int64
	LeaseExpiresAt                                               time.Time
}
type Version struct{ VersionID, AttemptID string }
type Inventory struct {
	Versions             []Version
	NextKey, NextVersion string
	Exhausted            bool
	Reason               string
}
type Commitment struct {
	Ticket                  Ticket
	CommitmentID, VersionID string
}
type AbsenceProof struct {
	VersionID string
	CheckedAt time.Time
	Absent    bool
}
type Repository interface {
	ClaimFileDelete(context.Context, string) (Ticket, bool, error)
	RecordFileDeleteInventory(context.Context, Ticket, Inventory) error
	GetFileDeleteInventory(context.Context, Ticket) (Inventory, error)
	CommitFileDeleteVersion(context.Context, Ticket, string) (Commitment, error)
	ClaimFileDeleteRecovery(context.Context, string) (Commitment, bool, error)
	SettleFileDeleteVersion(context.Context, Commitment, AbsenceProof) error
	FinalizeFileDelete(context.Context, Ticket) error
}

var canonicalID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func finite(t time.Time) bool { return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 }
func opaque(v string) bool {
	return len(v) > 0 && len(v) <= 1024 && utf8.ValidString(v) && strings.IndexFunc(v, unicode.IsControl) < 0
}
func fixedVersion(v string) bool { return v != "null" && opaque(v) }
func ValidateTicket(t Ticket) error {
	for _, id := range []string{t.JobID, t.TenantID, t.FileID, t.ConversationID, t.OwnerID, t.LeaseToken} {
		if !canonicalID.MatchString(id) {
			return ErrLeaseLost
		}
	}
	if t.PolicyVersion < 0 || t.StateVersion < 1 || !finite(t.LeaseExpiresAt) {
		return ErrLeaseLost
	}
	return nil
}
func ValidateInventory(t Ticket, in Inventory) error {
	if e := ValidateTicket(t); e != nil {
		return e
	}
	if len(in.Versions) > 100 || (in.Exhausted && (in.NextKey != "" || in.NextVersion != "")) || (!in.Exhausted && !opaque(in.NextKey)) || (in.NextVersion != "" && !fixedVersion(in.NextVersion)) {
		return ErrIncomplete
	}
	switch in.Reason {
	case "", "inventory_incomplete", "unknown_upload", "unknown_version", "delete_marker", "in_flight":
	default:
		return ErrIncomplete
	}
	seen := map[string]bool{}
	for _, v := range in.Versions {
		if !fixedVersion(v.VersionID) || (v.AttemptID != "" && !canonicalID.MatchString(v.AttemptID)) || seen[v.VersionID] {
			return ErrIncomplete
		}
		seen[v.VersionID] = true
	}
	return nil
}
func ValidateCommitment(c Commitment) error {
	if e := ValidateTicket(c.Ticket); e != nil {
		return e
	}
	if !canonicalID.MatchString(c.CommitmentID) || !fixedVersion(c.VersionID) {
		return ErrIncomplete
	}
	return nil
}
func ValidateAbsenceProof(c Commitment, p AbsenceProof) error {
	if e := ValidateCommitment(c); e != nil {
		return e
	}
	if p.VersionID != c.VersionID || !p.Absent || !finite(p.CheckedAt) {
		return ErrIncomplete
	}
	return nil
}
