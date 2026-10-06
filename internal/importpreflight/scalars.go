package importpreflight

import (
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

func normalizeUUID(s string) (string, bool) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return "", false
	}
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return "", false
	}
	if _, e := hex.DecodeString(h); e != nil {
		return "", false
	}
	return strings.ToLower(s), true
}

var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})$`)

func parseTimestamp(s string) (time.Time, bool) {
	if !timestampPattern.MatchString(s) || s[:4] == "0000" {
		return time.Time{}, false
	}
	if s[len(s)-1] != 'Z' {
		n := len(s)
		if s[n-5:n-3] > "23" || s[n-2:] > "59" {
			return time.Time{}, false
		}
	}
	v, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return time.Time{}, false
	}
	return v.UTC(), true
}
