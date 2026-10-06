package importpreflight

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

type Value struct {
	Valid  bool
	IsNull bool
	Text   string
	Bool   bool
	Time   *time.Time
}
type Record struct {
	Ordinal int
	Values  map[Field]Value
}

func (r Record) Text(f Field) (string, bool)     { v := r.Values[f]; return v.Text, v.Valid && !v.IsNull }
func (r Record) Bool(f Field) (bool, bool)       { v := r.Values[f]; return v.Bool, v.Valid && !v.IsNull }
func (r Record) Time(f Field) (*time.Time, bool) { v := r.Values[f]; return v.Time, v.Valid }

type Document struct {
	Tables                 map[Entity][]Record
	BaselineCommit         string
	DataOrigin             string
	ReferenceTime          time.Time
	IdentitySourceSelected bool
}

func normalizeValue(raw json.RawMessage, present bool, s fieldSpec) (Value, Code) {
	if !present {
		if s.nullable {
			return Value{Valid: true, IsNull: true}, ""
		}
		if s.hasDefault {
			return Value{Valid: true, Text: s.defaultText, Bool: s.defaultBool}, ""
		}
		return Value{}, "FIELD_REQUIRED"
	}
	if string(raw) == "null" {
		if s.nullable {
			return Value{Valid: true, IsNull: true}, ""
		}
		return Value{}, "TYPE_INVALID"
	}
	v := Value{Valid: true}
	if s.kind == "bool" {
		if string(raw) != "true" && string(raw) != "false" {
			return Value{}, "TYPE_INVALID"
		}
		v.Bool = string(raw) == "true"
		return v, ""
	}
	if json.Unmarshal(raw, &v.Text) != nil {
		return Value{}, "TYPE_INVALID"
	}
	if len(v.Text) > MaxString {
		return Value{}, "FIELD_LIMIT"
	}
	if strings.ContainsRune(v.Text, 0) {
		return Value{}, "TEXT_NUL"
	}
	if s.nonblank && strings.Trim(v.Text, " ") == "" {
		return Value{}, "TEXT_BLANK"
	}
	if s.kind == "uuid" {
		text, ok := normalizeUUID(v.Text)
		if !ok {
			return Value{}, "UUID_INVALID"
		}
		v.Text = text
	}
	if s.kind == "time" {
		tm, ok := parseTimestamp(v.Text)
		if !ok {
			return Value{}, "TIME_INVALID"
		}
		v.Time = &tm
		v.Text = ""
	}
	if len(s.enums) > 0 {
		ok := false
		for _, val := range s.enums {
			if v.Text == val {
				ok = true
			}
		}
		if !ok {
			return Value{}, "ENUM_INVALID"
		}
	}
	return v, ""
}
func Normalize(ctx context.Context, raw RawDocument, c *Collector) (Document, Counts, bool, error) {
	doc := Document{Tables: map[Entity][]Record{}}
	counts := Counts{}
	good := true
	total := 0
	add := func(e Entity, row int, f Field, code Code) {
		c.Add(Issue{Entity: e, Row: row, Field: f, Code: code})
		if e == "document" || code == "FIELD_LIMIT" {
			good = false
		}
	}
	for _, f := range headerFields {
		if f == "tables" {
			continue
		}
		if e := ContextFailure(ctx); e != nil {
			return doc, counts, false, e
		}
		b, ok := raw.Header[string(f)]
		if !ok {
			add("document", 0, f, "FIELD_REQUIRED")
			continue
		}
		if f == "format_version" {
			if strings.TrimSpace(string(b)) != "1" {
				add("document", 0, f, "FORMAT_VERSION_UNSUPPORTED")
			}
			continue
		}
		s := fieldSpec{kind: "text"}
		if f == "identity_source_selected" {
			s.kind = "bool"
		}
		if f == "reference_time" {
			s.kind = "time"
		}
		v, code := normalizeValue(b, true, s)
		if code != "" {
			add("document", 0, f, code)
			continue
		}
		switch f {
		case "baseline_commit":
			if len(v.Text) != 40 {
				add("document", 0, f, "TYPE_INVALID")
			} else if _, e := hex.DecodeString(v.Text); e != nil {
				add("document", 0, f, "TYPE_INVALID")
			} else {
				doc.BaselineCommit = v.Text
			}
		case "data_origin":
			if len(v.Text) == 0 {
				add("document", 0, f, "TEXT_BLANK")
			} else if len(v.Text) > 64 {
				add("document", 0, f, "FIELD_LIMIT")
			} else {
				doc.DataOrigin = v.Text
			}
		case "reference_time":
			doc.ReferenceTime = *v.Time
		case "identity_source_selected":
			doc.IdentitySourceSelected = v.Bool
		}
	}
	for _, e := range entities {
		rows, exists := raw.Tables[e]
		if !exists {
			add("document", 0, "tables", "FIELD_REQUIRED")
			continue
		}
		n := len(rows)
		counts[string(e)] = &n
		total += n
		doc.Tables[e] = []Record{}
		for i, row := range rows {
			if er := ContextFailure(ctx); er != nil {
				return doc, counts, false, er
			}
			r := Record{Ordinal: i + 1, Values: map[Field]Value{}}
			for _, f := range fields[e] {
				b, present := row[f]
				v, code := normalizeValue(b, present, specification(e, f))
				r.Values[f] = v
				if code != "" {
					add(e, i+1, f, code)
				}
			}
			doc.Tables[e] = append(doc.Tables[e], r)
		}
	}
	counts["total"] = &total
	return doc, counts, good, ContextFailure(ctx)
}
