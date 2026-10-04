package files

import (
	"testing"
	"time"
)

func TestFileRetentionBoundsAndUTC(t *testing.T) {
	p := DefaultRetentionPolicy()
	if p.Days != 365 || p.CleanupEnabled || p.Version != 0 {
		t.Fatal("unsafe default", p)
	}
	for _, days := range []int64{1, 365, 3650} {
		if _, e := NormalizeRetentionPolicy(RetentionPolicy{Days: days}); e != nil {
			t.Fatal(days, e)
		}
	}
	for _, days := range []int64{-1, 0, 3651} {
		if _, e := NormalizeRetentionPolicy(RetentionPolicy{Days: days}); e == nil {
			t.Fatal("invalid days accepted", days)
		}
	}
	if _, e := NormalizeRetentionPolicy(RetentionPolicy{Days: 365, Version: -1}); e == nil {
		t.Fatal("negative version accepted")
	}
	loc, e := time.LoadLocation("America/New_York")
	if e != nil {
		t.Fatal(e)
	}
	accepted := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	expiry, e := FileExpiresAt(accepted, 1)
	want := time.Date(2026, 3, 8, 17, 0, 0, 0, time.UTC)
	if e != nil || !expiry.Equal(want) {
		t.Fatal("retention used calendar day instead of 24h", expiry, e)
	}
	accepted = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	expiry, e = FileExpiresAt(accepted, 365)
	want = time.Date(2027, 10, 4, 0, 0, 0, 0, time.UTC)
	if e != nil || !expiry.Equal(want) {
		t.Fatal(expiry, e)
	}
	for _, at := range []time.Time{{}, time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, e := FileExpiresAt(at, 1); e == nil {
			t.Fatal("invalid time accepted", at)
		}
	}
}
