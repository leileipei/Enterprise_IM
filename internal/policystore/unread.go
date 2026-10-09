package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"strconv"
	"time"
)

// High-water marks are deliberately absent from this count. Retained batches
// are re-filtered at the final clock so expiry cannot inflate even a lower bound.
func (s Service) GetUnread(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string) (UnreadState, error) {
	wallStart := time.Now()
	clockStart := s.now()
	q, e := s.beginReceiptQuery(ctx, id, device, cid)
	if e != nil {
		return UnreadState{}, e
	}
	defer q.tx.Rollback(ctx)
	state := UnreadState{Exact: true}
	if e = q.tx.QueryRow(ctx, `SELECT c.last_seq,COALESCE(v.revision,0),COALESCE(p.current_version,0) FROM conversations c LEFT JOIN conversation_receipt_versions v ON v.tenant_id=c.tenant_id AND v.conversation_id=c.id LEFT JOIN policy_current p ON p.tenant_id=c.tenant_id WHERE c.tenant_id=$1 AND c.id=$2`, q.id.TenantID, q.cid).Scan(&state.HeadSeq, &state.ReceiptRevision, &state.PolicyVersion); e != nil {
		return UnreadState{}, e
	}

	var configuredMS int64
	if e = q.tx.QueryRow(ctx, `SELECT setting::bigint FROM pg_settings WHERE name='statement_timeout'`).Scan(&configuredMS); e != nil {
		return UnreadState{}, e
	}
	budgetTx := unreadBudgetTx{Tx: q.tx, configuredMS: configuredMS, remaining: func() time.Duration {
		return min(3*time.Second-time.Since(wallStart), 3*time.Second-s.now().Sub(clockStart))
	}}
	batches := []historyReadBatch{}
	read := map[int64]bool{}
	after := int64(0)
	inspected := 0
	for after < state.HeadSeq {
		if inspected >= 10000 || budgetTx.remaining() <= 0 {
			state.Exact = false
			break
		}
		if _, e = q.tx.Exec(ctx, "SAVEPOINT unread_page"); e != nil {
			return UnreadState{}, e
		}
		batch, pageRead, e := readUnreadPageTx(ctx, budgetTx, q, after, min(500, 10000-inspected), state.HeadSeq)
		if e != nil {
			if ctx.Err() != nil {
				return UnreadState{}, ctx.Err()
			}
			var pgerr *pgconn.PgError
			if !errors.Is(e, errUnreadBudget) && !(errors.As(e, &pgerr) && pgerr.Code == "57014") {
				return UnreadState{}, e
			}
			// Server statement cancellation aborts only this page. Recover the transaction
			// before auditing and checking authorization on the completed page prefix.
			if _, e = q.tx.Exec(ctx, "ROLLBACK TO SAVEPOINT unread_page"); e != nil {
				return UnreadState{}, e
			}
			if _, e = q.tx.Exec(ctx, "RELEASE SAVEPOINT unread_page"); e != nil {
				return UnreadState{}, e
			}
			state.Exact = false
			break
		}
		if _, e = q.tx.Exec(ctx, "RELEASE SAVEPOINT unread_page"); e != nil {
			return UnreadState{}, e
		}
		if len(batch.Page.Messages) == 0 {
			break
		}
		batches = append(batches, batch)
		for seq := range pageRead {
			read[seq] = true
		}
		for _, m := range batch.Page.Messages {
			if m.Seq <= state.HeadSeq {
				inspected++
			}
		}
		after = batch.Page.NextAfterSeq
		if !batch.Page.HasMore || after >= state.HeadSeq {
			break
		}
	}
	if _, e = q.tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, strconv.FormatInt(configuredMS, 10)+"ms"); e != nil {
		return UnreadState{}, e
	}
	if e = s.lockReceiptQueryDevice(ctx, &q); e != nil {
		return UnreadState{}, e
	}
	if e = auditReceiptTx(ctx, q.tx, q.id, q.cid, "message_unread_query", s.now()); e != nil {
		return UnreadState{}, e
	}
	var matched []policy.Rule
	if q.kind == "group" && len(batches) > 0 && hasGroupHistoryHardDeny(q.scope.Rules) {
		matched, e = groupHistoryMatchingDenials(ctx, q.tx, q.id, q.cid, q.scope.Actor, batches[0].ReaderIntervals, q.scope.Rules)
		if e != nil {
			return UnreadState{}, e
		}
	}
	if e = s.finalReceiptQuery(ctx, q); e != nil {
		return UnreadState{}, e
	}
	now := s.now()
	for _, batch := range batches {
		page, e := filterReceiptPage(q.scope, batch, q.kind, matched, now)
		if e != nil {
			return UnreadState{}, e
		}
		for _, m := range page.Messages {
			if m.Seq <= state.HeadSeq && !m.Redacted && m.SenderUserID != q.id.UserID && !read[m.Seq] {
				state.UnreadCount++
			}
		}
	}
	if !s.now().Before(q.expiry) {
		return UnreadState{}, ErrInvalidDevice
	}
	if e = q.tx.Commit(ctx); e != nil {
		return UnreadState{}, e
	}
	return state, nil
}

var errUnreadBudget = errors.New("unread scan budget exhausted")

type unreadBudgetTx struct {
	pgx.Tx
	remaining    func() time.Duration
	configuredMS int64
}

func (tx unreadBudgetTx) setBudget(ctx context.Context) error {
	remaining := tx.remaining()
	if remaining <= 0 {
		return errUnreadBudget
	}
	ms := max(int64(1), remaining.Milliseconds())
	if tx.configuredMS > 0 {
		ms = min(ms, tx.configuredMS)
	}
	_, e := tx.Tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, strconv.FormatInt(ms, 10)+"ms")
	return e
}
func (tx unreadBudgetTx) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	if e := tx.setBudget(ctx); e != nil {
		return nil, e
	}
	return tx.Tx.Query(ctx, q, args...)
}
func (tx unreadBudgetTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	if e := tx.setBudget(ctx); e != nil {
		return unreadErrorRow{e}
	}
	return tx.Tx.QueryRow(ctx, q, args...)
}

type unreadErrorRow struct{ err error }

func (r unreadErrorRow) Scan(...any) error { return r.err }
func readUnreadPageTx(ctx context.Context, tx pgx.Tx, q receiptQueryContext, after int64, limit int, head int64) (historyReadBatch, map[int64]bool, error) {
	batch, e := receiptHistoryBatchTx(ctx, tx, q.scope, q.cid, q.kind, after, limit, nil)
	if e != nil {
		return historyReadBatch{}, nil, e
	}
	// Unread authorization needs visibility evidence, never retained content.
	for i := range batch.Page.Messages {
		batch.Page.Messages[i].Text = ""
		batch.Page.Messages[i].Attachment = nil
	}
	read := map[int64]bool{}
	seqs := make([]int64, 0, len(batch.Page.Messages))
	for _, m := range batch.Page.Messages {
		if m.Seq <= head {
			seqs = append(seqs, m.Seq)
		}
	}
	if len(seqs) == 0 {
		return batch, read, nil
	}
	rows, e := tx.Query(ctx, `SELECT seq FROM message_user_receipts WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND seq=ANY($4::bigint[]) AND read_at IS NOT NULL`, q.id.TenantID, q.cid, q.id.UserID, seqs)
	if e != nil {
		return historyReadBatch{}, nil, e
	}
	for rows.Next() {
		var seq int64
		if e = rows.Scan(&seq); e != nil {
			rows.Close()
			return historyReadBatch{}, nil, e
		}
		read[seq] = true
	}
	e = rows.Err()
	rows.Close()
	return batch, read, e
}
