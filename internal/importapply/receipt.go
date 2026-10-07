package importapply

import (
	"bytes"
	"encoding/json"
	"errors"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"time"
	"unicode/utf8"
)

type TableCounts struct {
	Input     int `json:"input"`
	New       int `json:"new"`
	Identical int `json:"identical"`
	Conflict  int `json:"conflict"`
	Inserted  int `json:"inserted"`
}
type Issue struct {
	Entity p.Entity `json:"entity"`
	Row    int      `json:"row"`
	Field  p.Field  `json:"field"`
	Code   string   `json:"code"`
}

type State string
type Reason string

const (
	Applied                    State  = "applied"
	Rejected                   State  = "rejected"
	None                       Reason = "NONE"
	InputConflict              Reason = "INPUT_CONFLICT"
	DatabaseConstraintConflict Reason = "DATABASE_CONSTRAINT_CONFLICT"
)
const protocolVersion = "controlled_append_v1"

var errReceipt = errors.New("INVALID_IMPORT_RECEIPT")

type Receipt struct {
	ProtocolVersion string                 `json:"protocol_version"`
	State           State                  `json:"state"`
	Reason          Reason                 `json:"reason"`
	Counts          map[string]TableCounts `json:"counts"`
	Issues          []Issue                `json:"issues"`
	ErrorsTotal     int                    `json:"errors_total"`
	IssuesTruncated bool                   `json:"issues_truncated"`
	CompletedAt     time.Time              `json:"completed_at"`
}

func validateReceipt(r Receipt) error {
	if r.ProtocolVersion != protocolVersion || len(r.Counts) != 10 || r.Issues == nil || len(r.Issues) > p.MaxIssues || r.ErrorsTotal < len(r.Issues) || r.IssuesTruncated != (r.ErrorsTotal > len(r.Issues)) || r.CompletedAt.IsZero() || r.CompletedAt.Year() < 1 || r.CompletedAt.Year() > 9999 {
		return errReceipt
	}
	_, offset := r.CompletedAt.Zone()
	if offset != 0 {
		return errReceipt
	}
	if r.State == Applied {
		if r.Reason != None || r.ErrorsTotal != 0 {
			return errReceipt
		}
	} else if r.State == Rejected {
		if r.Reason != InputConflict && r.Reason != DatabaseConstraintConflict {
			return errReceipt
		}
	} else {
		return errReceipt
	}
	total := TableCounts{}
	for _, s := range p.Schema() {
		v, ok := r.Counts[string(s.Entity)]
		if !ok || v.Input < 0 || v.Input > p.MaxRows || v.New < 0 || v.Identical < 0 || v.Conflict < 0 || v.Inserted < 0 || v.Input != v.New+v.Identical+v.Conflict {
			return errReceipt
		}
		protected := s.Entity == "tenants" || s.Entity == "external_identities" || s.Entity == "admin_grants"
		if protected && (v.New != 0 || v.Inserted != 0) {
			return errReceipt
		}
		if r.State == Rejected && v.Inserted != 0 || r.State == Applied && (v.Conflict != 0 || v.Inserted != v.New) {
			return errReceipt
		}
		total.Input += v.Input
		total.New += v.New
		total.Identical += v.Identical
		total.Conflict += v.Conflict
		total.Inserted += v.Inserted
	}
	if r.Counts["total"] != total || total.Input > p.MaxRows || r.Reason == InputConflict && (r.ErrorsTotal == 0 || total.Conflict == 0) || r.Reason == DatabaseConstraintConflict && total.Conflict != 0 {
		return errReceipt
	}
	codes := map[string]bool{}
	for _, code := range []string{"GLOBAL_KEY_CONFLICT", "STORED_VALUE_DIFFERS", "STORED_UNIQUE_CONFLICT", "REF_NOT_FOUND", "REF_SCOPE_MISMATCH", "TREE_CYCLE", "SELF_PARENT", "VIRTUAL_LEGAL_MISMATCH", "VIRTUAL_MEMBERSHIP", "INTERVAL_OVERLAP", "PRIMARY_OVERLAP", "DEPARTMENT_INTERVAL_OUTSIDE", "GRANT_SCOPE_INVALID", "UNIQUE_DUPLICATE", "PK_DUPLICATE", "INTERVAL_INVALID", "PROTECTED_ENTITY_NEW"} {
		codes[code] = true
	}
	for _, i := range r.Issues {
		if !codes[i.Code] || i.Row < 1 || i.Row > r.Counts[string(i.Entity)].Input {
			return errReceipt
		}
		fieldOK := false
		for _, s := range p.Schema() {
			if s.Entity == i.Entity {
				fieldOK = i.Field == "_record"
				for _, f := range s.Fields {
					fieldOK = fieldOK || f.Name == i.Field
				}
			}
		}
		if !fieldOK {
			return errReceipt
		}
	}
	return nil
}
func EncodeReceipt(r Receipt) ([]byte, error) {
	if e := validateReceipt(r); e != nil {
		return nil, e
	}
	b, e := json.Marshal(r)
	if e != nil || len(b) > p.MaxReport {
		return nil, errReceipt
	}
	return b, nil
}
func DecodeReceipt(raw []byte) (Receipt, error) {
	fail := func() (Receipt, error) { return Receipt{}, errReceipt }
	if len(raw) > p.MaxReport || !utf8.Valid(raw) {
		return fail()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSON(d, 0); err != nil {
		return fail()
	}
	if _, err := d.Token(); err != io.EOF {
		return fail()
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || !exactFields(root, []string{"protocol_version", "state", "reason", "counts", "issues", "errors_total", "issues_truncated", "completed_at"}) {
		return fail()
	}
	var counts map[string]map[string]json.RawMessage
	if json.Unmarshal(root["counts"], &counts) != nil {
		return fail()
	}
	for _, v := range counts {
		if !exactFields(v, []string{"input", "new", "identical", "conflict", "inserted"}) {
			return fail()
		}
	}
	var issues []map[string]json.RawMessage
	if json.Unmarshal(root["issues"], &issues) != nil {
		return fail()
	}
	for _, v := range issues {
		if !exactFields(v, []string{"entity", "row", "field", "code"}) {
			return fail()
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var r Receipt
	if d.Decode(&r) != nil || validateReceipt(r) != nil {
		return fail()
	}
	return r, nil
}
func exactFields(m map[string]json.RawMessage, fields []string) bool {
	if len(m) != len(fields) {
		return false
	}
	for _, k := range fields {
		v, ok := m[k]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errReceipt
	}
	tok, e := d.Token()
	if e != nil {
		return errReceipt
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			s, ok := key.(string)
			if e != nil || !ok || seen[s] {
				return errReceipt
			}
			seen[s] = true
			if e = uniqueJSON(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return errReceipt
		}
	case '[':
		for d.More() {
			if e = uniqueJSON(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return errReceipt
		}
	default:
		return errReceipt
	}
	return nil
}
