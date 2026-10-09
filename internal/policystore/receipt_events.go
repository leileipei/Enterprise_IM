package policystore

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

func recordReceiptUserChangesTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, cid string, changedSeqs []int64) (int64, error) {
	if len(changedSeqs) == 0 {
		var revision int64
		e := tx.QueryRow(ctx, "SELECT COALESCE((SELECT revision FROM conversation_receipt_versions WHERE tenant_id=$1 AND conversation_id=$2),0)", id.TenantID, cid).Scan(&revision)
		return revision, e
	}
	if _, e := tx.Exec(ctx, "INSERT INTO conversation_receipt_versions(tenant_id,conversation_id,revision) VALUES($1,$2,0) ON CONFLICT(tenant_id,conversation_id) DO NOTHING", id.TenantID, cid); e != nil {
		return 0, e
	}
	var revision int64
	if e := tx.QueryRow(ctx, "SELECT revision FROM conversation_receipt_versions WHERE tenant_id=$1 AND conversation_id=$2 FOR UPDATE", id.TenantID, cid).Scan(&revision); e != nil {
		return 0, e
	}
	if revision == math.MaxInt64 {
		return 0, errors.New("receipt revision exhausted")
	}
	revision++
	if _, e := tx.Exec(ctx, "UPDATE conversation_receipt_versions SET revision=$3 WHERE tenant_id=$1 AND conversation_id=$2", id.TenantID, cid, revision); e != nil {
		return 0, e
	}
	var eventID string
	if e := tx.QueryRow(ctx, "INSERT INTO receipt_outbox_events(tenant_id,conversation_id,revision,actor_user_id) VALUES($1,$2,$3,$4) RETURNING id::text", id.TenantID, cid, revision, id.UserID).Scan(&eventID); e != nil {
		return 0, e
	}
	_, e := tx.Exec(ctx, `INSERT INTO receipt_outbox_targets(tenant_id,event_id,target_user_id)
 SELECT $1,$2,target FROM (SELECT sender_user_id AS target FROM messages WHERE tenant_id=$1 AND conversation_id=$3 AND seq=ANY($4::bigint[]) UNION SELECT $5::uuid) recipients ORDER BY target`, id.TenantID, eventID, cid, changedSeqs, id.UserID)
	return revision, e
}
