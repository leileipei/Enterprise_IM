package importpreflight

import (
	"bytes"
	"context"
	"encoding/json"
)

type RawRow map[Field]json.RawMessage
type RawDocument struct {
	Header map[string]json.RawMessage
	Tables map[Entity][]RawRow
}

func DecodeRaw(ctx context.Context, raw []byte, c *Collector) (RawDocument, bool, error) {
	doc := RawDocument{Header: map[string]json.RawMessage{}, Tables: map[Entity][]RawRow{}}
	if e := ContextFailure(ctx); e != nil {
		return doc, false, e
	}
	if len(raw) > MaxInput {
		c.Add(Issue{Entity: "document", Field: "_document", Code: "INPUT_TOO_LARGE"})
		return doc, false, nil
	}
	if e := scanJSON(ctx, raw, c); e != nil {
		f := e.(Failure)
		if f.Code == "CANCELED" || f.Code == "TIMEOUT" {
			return doc, false, e
		}
		if f.Code != "FIELD_LIMIT" {
			c.Add(Issue{Entity: "document", Field: "_document", Code: f.Code})
		}
		return doc, false, nil
	}
	// Duplicate members are rejected before any map can overwrite their values.
	if c.total > 0 {
		return doc, false, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	good := true
	total := 0
	add := func(ent Entity, row int, f Field, code Code) {
		good = false
		c.Add(Issue{Entity: ent, Row: row, Field: f, Code: code})
	}
	discard := func() { var v json.RawMessage; _ = d.Decode(&v) }
	tok, _ := d.Token()
	if tok != json.Delim('{') {
		add("document", 0, "_document", "TYPE_INVALID")
		return doc, false, nil
	}
	for d.More() {
		if e := ContextFailure(ctx); e != nil {
			return doc, false, e
		}
		k, _ := d.Token()
		s := k.(string)
		f, known := knownField("document", s)
		if !known {
			add("document", 0, "_unknown", "UNKNOWN_FIELD")
			discard()
			continue
		}
		if f != "tables" {
			var v json.RawMessage
			_ = d.Decode(&v)
			doc.Header[s] = v
			continue
		}
		tok, _ = d.Token()
		if tok != json.Delim('{') {
			add("document", 0, "tables", "TYPE_INVALID")
			return doc, false, nil
		}
		doc.Header["tables"] = json.RawMessage(`{}`)
		for d.More() {
			k, _ = d.Token()
			e, ok := knownEntity(k.(string))
			if !ok {
				add("document", 0, "_unknown", "UNKNOWN_TABLE")
				discard()
				continue
			}
			tok, _ = d.Token()
			if tok != json.Delim('[') {
				add(e, 0, "_record", "TYPE_INVALID")
				return doc, false, nil
			}
			doc.Tables[e] = []RawRow{}
			row := 0
			for d.More() {
				if e2 := ContextFailure(ctx); e2 != nil {
					return doc, false, e2
				}
				row++
				total++
				if total > MaxRows {
					add("document", 0, "_document", "ROW_LIMIT")
					return doc, false, nil
				}
				tok, _ = d.Token()
				if tok != json.Delim('{') {
					add(e, row, "_record", "TYPE_INVALID")
					return doc, false, nil
				}
				r := RawRow{}
				for d.More() {
					k, _ = d.Token()
					rf, ok := knownField(e, k.(string))
					if !ok {
						add(e, row, "_unknown", "UNKNOWN_FIELD")
						discard()
						continue
					}
					var v json.RawMessage
					_ = d.Decode(&v)
					r[rf] = v
				}
				_, _ = d.Token()
				doc.Tables[e] = append(doc.Tables[e], r)
			}
			_, _ = d.Token()
		}
		_, _ = d.Token()
	}
	_, _ = d.Token()
	for _, f := range headerFields {
		if _, ok := doc.Header[string(f)]; !ok {
			add("document", 0, f, "FIELD_REQUIRED")
		}
	}
	for _, e := range entities {
		if _, ok := doc.Tables[e]; !ok {
			add(e, 0, "_record", "FIELD_REQUIRED")
		}
	}
	return doc, good, nil
}
