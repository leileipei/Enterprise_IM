package policystore

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func testUUIDv7At(at time.Time) string {
	millis := uint64(at.UnixMilli())
	return fmt.Sprintf("%08x-%04x-7000-8000-000000000001", millis>>16, millis&0xffff)
}

func TestValidateClientMessageIDWindowAndVersion(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{testUUIDv7At(now), testUUIDv7At(now.Add(-7 * 24 * time.Hour)),
		testUUIDv7At(now.Add(5 * time.Minute))} {
		if err := validateClientMessageID(id, now); err != nil {
			t.Fatalf("valid UUIDv7 %s: %v", id, err)
		}
	}
	if err := validateClientMessageID(testUUIDv7At(now.Add(-7*24*time.Hour-time.Millisecond)), now); !errors.Is(err, ErrRetryExpired) {
		t.Fatalf("expired UUIDv7: %v", err)
	}
	for _, id := range []string{"bad", "00000000-0000-4000-8000-000000000001",
		"0199f04a-0000-7000-7000-000000000001", testUUIDv7At(now.Add(5*time.Minute + time.Millisecond))} {
		if err := validateClientMessageID(id, now); !errors.Is(err, ErrInvalidClientMessageID) {
			t.Fatalf("invalid UUIDv7 %s: %v", id, err)
		}
	}
}
