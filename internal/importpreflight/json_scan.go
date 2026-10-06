package importpreflight

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

func scanEncoding(ctx context.Context, raw []byte) error {
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return Failure{"INVALID_ENCODING"}
	}
	inside := false
	for i := 0; i < len(raw); i++ {
		if i%4096 == 0 {
			if e := ContextFailure(ctx); e != nil {
				return e
			}
		}
		b := raw[i]
		if !inside {
			if b == '"' {
				inside = true
			}
			continue
		}
		if b == '"' {
			inside = false
			continue
		}
		if b != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			break
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			break
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return Failure{"JSON_INVALID"}
		}
		i += 4
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return Failure{"INVALID_ENCODING"}
			}
			m, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || m < 0xdc00 || m > 0xdfff {
				return Failure{"INVALID_ENCODING"}
			}
			i += 6
		} else if n >= 0xdc00 && n <= 0xdfff {
			return Failure{"INVALID_ENCODING"}
		}
	}
	return ContextFailure(ctx)
}

type scanLocation struct {
	entity Entity
	row    int
	field  Field
	mode   string
}

func (l scanLocation) issue(code Code) Issue {
	return Issue{Entity: l.entity, Row: l.row, Field: l.field, Code: code}
}
func scanJSON(ctx context.Context, raw []byte, c *Collector) error {
	if e := scanEncoding(ctx, raw); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	totalRows := 0
	var value func(scanLocation, int) error
	value = func(l scanLocation, depth int) error {
		if e := ContextFailure(ctx); e != nil {
			return e
		}
		tok, e := d.Token()
		if e != nil {
			return Failure{"JSON_INVALID"}
		}
		switch v := tok.(type) {
		case json.Delim:
			if v != '{' && v != '[' {
				return Failure{"JSON_INVALID"}
			}
			if depth > MaxDepth {
				return Failure{"DEPTH_LIMIT"}
			}
			if v == '{' {
				seen := map[string]bool{}
				for d.More() {
					key, e := d.Token()
					if e != nil {
						return Failure{"JSON_INVALID"}
					}
					s, ok := key.(string)
					if !ok {
						return Failure{"JSON_INVALID"}
					}
					child := l
					child.mode = ""
					if f, ok := knownField(l.entity, s); ok {
						child.field = f
					} else {
						child.field = "_unknown"
					}
					if seen[s] {
						c.Add(child.issue("DUPLICATE_JSON_KEY"))
					}
					seen[s] = true
					if l.mode == "root" && s == "tables" {
						child.mode = "tables"
					} else if l.mode == "tables" {
						if ent, ok := knownEntity(s); ok {
							child = scanLocation{ent, 0, "_record", "rows"}
						}
					}
					if e := value(child, depth+1); e != nil {
						return e
					}
				}
			} else {
				row := 0
				for d.More() {
					row++
					child := l
					child.mode = ""
					if l.mode == "rows" {
						totalRows++
						if totalRows > MaxRows {
							return Failure{"ROW_LIMIT"}
						}
						child.row = row
					}
					if e := value(child, depth+1); e != nil {
						return e
					}
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(map[json.Delim]rune{'{': '}', '[': ']'}[v]) {
				return Failure{"JSON_INVALID"}
			}
		case string:
			if len(v) > MaxString {
				c.Add(l.issue("FIELD_LIMIT"))
				return Failure{"FIELD_LIMIT"}
			}
		}
		return nil
	}
	if e := value(scanLocation{"document", 0, "_document", "root"}, 1); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return Failure{"JSON_INVALID"}
	}
	return ContextFailure(ctx)
}
