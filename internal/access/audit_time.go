package access

import (
	"regexp"
	"time"
)

// Microsecond precision matches PostgreSQL timestamptz without rounding a filter boundary.
var auditTimePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

// ParseAuditTimeRange accepts optional inclusive from and exclusive until bounds.
// Nonempty timestamps require an explicit zone and a UTC year within 1..9999.
func ParseAuditTimeRange(from, until string) (*time.Time, *time.Time, error) {
	parse := func(value string) (*time.Time, error) {
		if value == "" {
			return nil, nil
		}
		if !auditTimePattern.MatchString(value) {
			return nil, ErrInvalidAuditQuery
		}
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || at.Year() < 1 {
			return nil, ErrInvalidAuditQuery
		}
		at = at.UTC()
		if at.Year() < 1 || at.Year() > 9999 {
			return nil, ErrInvalidAuditQuery
		}
		return &at, nil
	}
	start, err := parse(from)
	if err != nil {
		return nil, nil, err
	}
	end, err := parse(until)
	if err != nil {
		return nil, nil, err
	}
	if start != nil && end != nil && !start.Before(*end) {
		return nil, nil, ErrInvalidAuditQuery
	}
	return start, end, nil
}

func auditTimeText(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}
