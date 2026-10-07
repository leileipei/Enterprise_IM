package importapply

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"strings"
	"time"
)

func (s *Service) Get(ctx context.Context, p access.ImportPrincipal, requestID string) (Result, error) {
	fail := func(e error) (Result, error) { return Result{}, publicError(e) }
	p = principal(p)
	ctx, cancel, e := requestContext(ctx, p)
	if e != nil {
		return fail(ErrForbidden)
	}
	defer cancel()
	if !uuidPattern.MatchString(requestID) {
		return fail(ErrInvalidInput)
	}
	requestID = strings.ToLower(requestID)
	if e = s.Preauthorize(ctx, p); e != nil {
		return fail(e)
	}
	b := budgetFrom(ctx)
	conn, e := s.pool.Acquire(ctx)
	if e != nil {
		return fail(e)
	}
	var tx pgx.Tx
	defer func() { cleanup(conn, tx, false, 0, b) }()
	tx, e = begin(ctx, conn, pgx.ReadCommitted, b)
	if e != nil {
		return fail(e)
	}
	m := newMeteredTx(tx, b)
	if _, e = m.Exec(ctx, "SET LOCAL lock_timeout='1s'"); e != nil {
		return fail(e)
	}
	if e = access.AuthorizeImport(ctx, m, s.schema, p, time.Now()); e != nil {
		return fail(e)
	}
	var acquired bool
	if e = m.QueryRow(ctx, "SELECT pg_catalog.pg_try_advisory_xact_lock($1)", batchLockKey(p.Identity.TenantID, requestID)).Scan(&acquired); e != nil {
		return fail(e)
	}
	if !acquired {
		return fail(ErrBusy)
	}
	stored, found, e := lookupReceipt(ctx, m, s.schema, p.Identity.TenantID, requestID)
	if e != nil {
		return fail(e)
	}
	reason := "NONE"
	if !found {
		reason = "NOT_RECORDED"
	}
	if _, e = m.Exec(ctx, "INSERT INTO "+tableName(s.schema, "audit_events")+" (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,'controlled_import.query','import_batch',$4,'allow',$5,$6)", p.Identity.TenantID, p.Identity.UserID, p.Identity.ActingMembershipID, requestID, reason, time.Now().UTC()); e != nil {
		return fail(ErrAuditUnavailable)
	}
	if e = access.AuthorizeImport(ctx, m, s.schema, p, time.Now()); e != nil {
		return fail(e)
	}
	var encoded []byte
	if found {
		encoded, e = EncodeReceipt(stored.Receipt)
		if e != nil {
			return fail(e)
		}
	}
	if e = commit(ctx, tx, b); e != nil {
		return fail(e)
	}
	if !found {
		return fail(ErrNotRecorded)
	}
	return Result{stored.Receipt, encoded, true}, nil
}
