package policystore

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"reflect"
	"testing"
	"time"
)

// Test-only bridge lets the external SQL tests reuse the existing migration fixtures.
var ExistingTypedMessageACKForTest = existingTypedMessageACK
var FileMessageDigestForTest = fileMessageDigest

type proofTx struct {
	pgx.Tx
	row     pgx.Row
	queries int
}

func (tx *proofTx) QueryRow(context.Context, string, ...any) pgx.Row { tx.queries++; return tx.row }

type proofRow struct {
	values []any
	err    error
}

func (r proofRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dst) != len(r.values) {
		return errors.New("unexpected proof column count")
	}
	for i, v := range r.values {
		if v != nil {
			reflect.ValueOf(dst[i]).Elem().Set(reflect.ValueOf(v))
		}
	}
	return nil
}
func TestFileMessageReplayEvidenceInvalidRows(t *testing.T) {
	id := access.TrustedIdentity{TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa01", UserID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa02", ActingMembershipID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa03"}
	req := MessageSendRequest{ClientMessageID: contentClient, MessageType: "file", FileID: contentFile, Caption: "caption"}
	var fp [32]byte
	fp[0] = 1
	digest, e := fileMessageDigest(id, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa04", req, fp)
	if e != nil {
		t.Fatal(e)
	}
	mid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa05"
	fid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	member := id.ActingMembershipID
	// Order is the shared single-statement projection, including all three proof states.
	good := []any{digest[:], digest[:], nil, nil, &mid, mid, int64(1), time.Now().UTC(), "file", &fid, fp[:], nil, member}
	for _, tc := range []struct {
		name  string
		index int
		value any
	}{
		{"missing key", 4, nil}, {"missing message digest", 0, nil}, {"short key digest", 1, []byte{1}},
		{"different key digest", 1, sha256Sum("different")}, {"missing link", 9, nil}, {"short fingerprint", 10, []byte{1}},
		{"missing sender membership", 12, ""}, {"wrong kind", 8, "unknown"},
		{"only message retired", 2, timePointer(time.Now())}, {"only key retired", 3, timePointer(time.Now())},
		{"only fingerprint retired", 11, timePointer(time.Now())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := append([]any(nil), good...)
			v[tc.index] = tc.value
			tx := &proofTx{row: proofRow{values: v}}
			ack, exists, same, err := existingTypedMessageACK(context.Background(), tx, id, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa04", req)
			if !errors.Is(err, errInvalidMessageIdempotency) || !exists || same || ack.MessageID != "" || tx.queries != 1 {
				t.Fatalf("corrupt proof: %+v %v %v %v queries=%d", ack, exists, same, err, tx.queries)
			}
		})
	}
	tx := &proofTx{row: proofRow{err: pgx.ErrNoRows}}
	_, exists, _, err := existingTypedMessageACK(context.Background(), tx, id, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa04", req)
	if err != nil || exists || tx.queries != 1 {
		t.Fatal(exists, err)
	}
	stamp := time.Now().UTC()
	retired := append([]any(nil), good...)
	retired[0] = nil
	retired[1] = nil
	retired[10] = nil
	retired[2] = &stamp
	retired[3] = &stamp
	retired[11] = &stamp
	tx = &proofTx{row: proofRow{values: retired}}
	ack, exists, same, err := existingTypedMessageACK(context.Background(), tx, id, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa04", req)
	if !errors.Is(err, ErrRetryExpired) || !exists || same || ack.MessageID != "" {
		t.Fatal(ack, exists, same, err)
	}
}
func sha256Sum(v string) []byte          { d := sha256.Sum256([]byte(v)); return d[:] }
func timePointer(v time.Time) *time.Time { return &v }
