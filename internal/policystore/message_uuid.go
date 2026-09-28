package policystore

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidClientMessageID = errors.New("invalid client message id")
	ErrRetryExpired           = errors.New("client message retry window expired")
)

func validateClientMessageID(id string, now time.Time) error {
	if !directoryUUIDPattern.MatchString(id) || id[14] != '7' ||
		!strings.ContainsRune("89abAB", rune(id[19])) {
		return ErrInvalidClientMessageID
	}
	bytes, err := hex.DecodeString(id[:8] + id[9:13])
	if err != nil {
		return ErrInvalidClientMessageID
	}
	var milliseconds int64
	for _, b := range bytes {
		milliseconds = milliseconds<<8 | int64(b)
	}
	issuedAt := time.UnixMilli(milliseconds)
	if issuedAt.Before(now.Add(-7 * 24 * time.Hour)) {
		return ErrRetryExpired
	}
	if issuedAt.After(now.Add(5 * time.Minute)) {
		return ErrInvalidClientMessageID
	}
	return nil
}
