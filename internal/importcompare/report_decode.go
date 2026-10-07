package importcompare

import (
	"bytes"
	"context"
	"encoding/json"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
)

func strict(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return dbFailure("DATABASE_READ_FAILED")
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return dbFailure("DATABASE_READ_FAILED")
	}
	return nil
}
func (i *Issue) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Stage   Stage    `json:"stage"`
		Entity  p.Entity `json:"entity"`
		Row     *int     `json:"row"`
		Field   p.Field  `json:"field"`
		Code    p.Code   `json:"code"`
		Related *int     `json:"related_row"`
	}
	if err := strict(raw, &wire); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, k := range []string{"stage", "entity", "row", "field", "code"} {
		if _, ok := fields[k]; !ok {
			return dbFailure("DATABASE_READ_FAILED")
		}
	}
	*i = Issue{wire.Stage, p.Issue{Entity: wire.Entity, Field: wire.Field, Code: wire.Code}}
	if wire.Row != nil {
		i.Issue.Row = *wire.Row
	}
	if wire.Related != nil {
		i.Issue.RelatedRow = *wire.Related
	}
	return nil
}
func DecodeReport(ctx context.Context, raw []byte) (Report, error) {
	bad := func() (Report, error) { return Report{}, dbFailure("DATABASE_READ_FAILED") }
	if len(raw) == 0 || len(raw) > p.MaxReport {
		return bad()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return dbFailure("DATABASE_READ_FAILED")
		}
		if err := p.ContextFailure(ctx); err != nil {
			return err
		}
		token, e := decoder.Token()
		if e != nil {
			return e
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					key, e := decoder.Token()
					if e != nil {
						return e
					}
					k, ok := key.(string)
					if !ok || seen[k] {
						return dbFailure("DATABASE_READ_FAILED")
					}
					seen[k] = true
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
			case '[':
				for decoder.More() {
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
			default:
				return dbFailure("DATABASE_READ_FAILED")
			}
			_, e = decoder.Token()
			return e
		}
		return nil
	}
	if err := walk(0); err != nil {
		return bad()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return bad()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 18 {
		return bad()
	}
	for _, key := range []string{"report_version", "validation_profile", "scope", "status", "file_status", "file_checks_complete", "checks_complete", "database_checked", "input_sha256", "counts", "classification_counts", "errors_total", "issues", "issues_truncated", "identity_provider_checked", "additional_database_rules_checked", "write_concurrency_checked", "import_authorized"} {
		if _, ok := fields[key]; !ok {
			return bad()
		}
	}
	var r Report
	if strict(raw, &r) != nil || validateReport(r) != nil {
		return bad()
	}
	return r, nil
}
