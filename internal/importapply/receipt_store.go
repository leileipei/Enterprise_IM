package importapply

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type BatchBinding struct {
	TenantID, RequestID, ActorUserID, ActingMembershipID string
	ProtocolVersion                                      string
	InputSHA256                                          [32]byte
}
type StoredBatch struct {
	Binding BatchBinding
	Receipt Receipt
}

func (BatchBinding) MarshalJSON() ([]byte, error) { return nil, errReceipt }
func (StoredBatch) MarshalJSON() ([]byte, error)  { return nil, errReceipt }
func (b BatchBinding) String() string             { return "<private import binding>" }
func (b StoredBatch) String() string              { return "<private import batch>" }
func MatchBinding(a, b BatchBinding) bool         { return a == b }
func tableName(schema, table string) string       { return pgx.Identifier{schema, table}.Sanitize() }
func lookupReceipt(ctx context.Context, tx pgx.Tx, schema, tenantID, requestID string) (StoredBatch, bool, error) {
	var b StoredBatch
	var digest, raw []byte
	var state, reason string
	var completed time.Time
	e := tx.QueryRow(ctx, "SELECT tenant_id::text,request_id::text,actor_user_id::text,acting_membership_id::text,protocol_version,input_sha256,state,reason,receipt,completed_at FROM "+tableName(schema, "import_batches")+" WHERE tenant_id=$1 AND request_id=$2", tenantID, requestID).Scan(&b.Binding.TenantID, &b.Binding.RequestID, &b.Binding.ActorUserID, &b.Binding.ActingMembershipID, &b.Binding.ProtocolVersion, &digest, &state, &reason, &raw, &completed)
	if errors.Is(e, pgx.ErrNoRows) {
		return StoredBatch{}, false, nil
	}
	if e != nil {
		return StoredBatch{}, false, e
	}
	if len(digest) != 32 {
		return StoredBatch{}, false, errReceipt
	}
	copy(b.Binding.InputSHA256[:], digest)
	b.Receipt, e = DecodeReceipt(raw)
	if e != nil {
		return StoredBatch{}, false, e
	}
	if b.Binding.ProtocolVersion != b.Receipt.ProtocolVersion || state != string(b.Receipt.State) || reason != string(b.Receipt.Reason) || !completed.Equal(b.Receipt.CompletedAt) {
		return StoredBatch{}, false, errReceipt
	}
	return b, true, nil
}
func insertReceipt(ctx context.Context, tx pgx.Tx, schema string, batch StoredBatch) error {
	b, e := EncodeReceipt(batch.Receipt)
	if e != nil {
		return e
	}
	if batch.Binding.ProtocolVersion != batch.Receipt.ProtocolVersion {
		return errReceipt
	}
	tag, e := tx.Exec(ctx, "INSERT INTO "+tableName(schema, "import_batches")+" (tenant_id,request_id,actor_user_id,acting_membership_id,protocol_version,input_sha256,state,reason,receipt,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", batch.Binding.TenantID, batch.Binding.RequestID, batch.Binding.ActorUserID, batch.Binding.ActingMembershipID, batch.Binding.ProtocolVersion, batch.Binding.InputSHA256[:], batch.Receipt.State, batch.Receipt.Reason, b, batch.Receipt.CompletedAt)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return errReceipt
	}
	return nil
}

func (b BatchBinding) GoString() string { return b.String() }
func (b StoredBatch) GoString() string  { return b.String() }
