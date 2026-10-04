package files

import (
	"errors"
	"time"
)

var ErrInvalidRetentionPolicy = errors.New("invalid file retention policy")

type RetentionPolicy struct {
	Days           int64
	CleanupEnabled bool
	Version        int64
}

func DefaultRetentionPolicy() RetentionPolicy { return RetentionPolicy{Days: 365} }
func NormalizeRetentionPolicy(p RetentionPolicy) (RetentionPolicy, error) {
	if p.Days < 1 || p.Days > 3650 || p.Version < 0 {
		return RetentionPolicy{}, ErrInvalidRetentionPolicy
	}
	return p, nil
}

// FileExpiresAt uses an elapsed UTC duration, independent of local calendar/DST.
func FileExpiresAt(acceptedAt time.Time, days int64) (time.Time, error) {
	if acceptedAt.IsZero() || acceptedAt.UTC().Year() < 1 || acceptedAt.UTC().Year() > 9999 || days < 1 || days > 3650 {
		return time.Time{}, ErrInvalidRetentionPolicy
	}
	expiry := acceptedAt.UTC().Add(time.Duration(days) * 24 * time.Hour)
	if !expiry.After(acceptedAt) || expiry.Year() > 9999 {
		return time.Time{}, ErrInvalidRetentionPolicy
	}
	return expiry, nil
}
