package importapply

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"testing"
	"time"
)

func receiptFixture() Receipt {
	r := Receipt{ProtocolVersion: "controlled_append_v1", State: Applied, Reason: None, Counts: map[string]TableCounts{}, Issues: []Issue{}, CompletedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	for _, s := range p.Schema() {
		r.Counts[string(s.Entity)] = TableCounts{}
	}
	r.Counts["users"] = TableCounts{Input: 2, New: 1, Identical: 1, Inserted: 1}
	r.Counts["total"] = r.Counts["users"]
	return r
}
func TestAppendReceiptContract(t *testing.T) {
	r := receiptFixture()
	b, e := EncodeReceipt(r)
	if e != nil {
		t.Fatal(e)
	}
	d, e := DecodeReceipt(b)
	if e != nil || d.Counts["users"].Inserted != 1 {
		t.Fatal("receipt lost insert")
	}
	r.State = Rejected
	r.Reason = DatabaseConstraintConflict
	if _, e := EncodeReceipt(r); e == nil {
		t.Fatal("rejected receipt claims inserts")
	}
	for k, v := range r.Counts {
		v.Inserted = 0
		r.Counts[k] = v
	}
	if _, e := EncodeReceipt(r); e != nil {
		t.Fatal(e)
	}
	r = receiptFixture()
	r.Counts["admin_grants"] = TableCounts{Input: 1, New: 1, Inserted: 1}
	r.Counts["total"] = TableCounts{Input: 3, New: 2, Identical: 1, Inserted: 2}
	if _, e := EncodeReceipt(r); e == nil {
		t.Fatal("protected insert receipt")
	}
	r = receiptFixture()
	r.Counts["users"] = TableCounts{Input: 2, New: 2, Inserted: 1}
	if _, e := EncodeReceipt(r); e == nil {
		t.Fatal("partial insert marked applied")
	}
}
func TestAppendReceiptStrictDecode(t *testing.T) {
	b, e := EncodeReceipt(receiptFixture())
	if e != nil {
		t.Fatal(e)
	}
	malformed := [][]byte{[]byte(`{`), append(b, []byte(`{}`)...), bytes.Replace(b, []byte(`"errors_total":0`), []byte(`"errors_total":0,"errors_total":0`), 1), bytes.Replace(b, []byte(`"errors_total":0,`), nil, 1), bytes.Replace(b, []byte(`"protocol_version"`), []byte(`"unknown"`), 1), bytes.Repeat([]byte(" "), p.MaxReport+1), bytes.Replace(b, []byte(`"issues":[]`), []byte(`"issues":null`), 1)}
	for i, raw := range malformed {
		if _, err := DecodeReceipt(raw); err == nil {
			t.Fatalf("case %d accepted malformed receipt", i)
		}
	}
}
func TestAppendRawBinding(t *testing.T) {
	a := BatchBinding{TenantID: "t", RequestID: "r", ActorUserID: "u", ActingMembershipID: "m", ProtocolVersion: "controlled_append_v1", InputSHA256: sha256.Sum256([]byte(`{}`))}
	b := a
	if !MatchBinding(a, b) {
		t.Fatal("same bytes differ")
	}
	b.InputSHA256 = sha256.Sum256([]byte(`{ }`))
	if MatchBinding(a, b) {
		t.Fatal("whitespace hash ignored")
	}
	b = a
	b.ActingMembershipID = "other"
	if MatchBinding(a, b) {
		t.Fatal("membership ignored")
	}
	if _, err := json.Marshal(a); err == nil {
		t.Fatal("binding leaked")
	}
	if _, err := json.Marshal(StoredBatch{Binding: a, Receipt: receiptFixture()}); err == nil {
		t.Fatal("stored batch leaked")
	}
}
