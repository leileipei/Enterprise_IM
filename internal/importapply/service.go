package importapply

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"regexp"
	"strings"
	"time"
)

type Service struct {
	pool   *pgxpool.Pool
	schema string
}
type Result struct {
	Receipt Receipt
	Encoded []byte
	Replay  bool
}

func NewService(pool *pgxpool.Pool, schema string) (*Service, error) {
	if pool == nil || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(schema) {
		return nil, ErrDatabaseUnavailable
	}
	return &Service{pool, schema}, nil
}
func principal(p access.ImportPrincipal) access.ImportPrincipal {
	p.Identity.TenantID = strings.ToLower(p.Identity.TenantID)
	p.Identity.UserID = strings.ToLower(p.Identity.UserID)
	p.Identity.ActingMembershipID = strings.ToLower(p.Identity.ActingMembershipID)
	return p
}
func requestContext(ctx context.Context, p access.ImportPrincipal) (context.Context, context.CancelFunc, error) {
	if budgetFrom(ctx) != nil {
		return ctx, func() {}, nil
	}
	return RequestContext(ctx, time.Now(), p.ExpiresAt)
}
func (s *Service) CheckReady(ctx context.Context) error {
	ctx, cancel, e := RequestContext(ctx, time.Now(), time.Now().Add(30*time.Second))
	if e != nil {
		return ErrDatabaseUnavailable
	}
	defer cancel()
	b := budgetFrom(ctx)
	conn, e := s.pool.Acquire(ctx)
	if e != nil {
		return publicError(e)
	}
	var tx pgx.Tx
	defer func() { cleanup(conn, tx, false, 0, b) }()
	tx, e = begin(ctx, conn, pgx.Serializable, b)
	if e != nil {
		return publicError(e)
	}
	m := newMeteredTx(tx, b)
	for _, sql := range []string{"SET LOCAL search_path=pg_catalog", "SET LOCAL lock_timeout='1s'"} {
		if _, e = m.Exec(ctx, sql); e != nil {
			return publicError(e)
		}
	}
	return publicError(c.CheckAppendProfile(ctx, m, s.schema))
}
func (s *Service) Preauthorize(ctx context.Context, p access.ImportPrincipal) error {
	p = principal(p)
	ctx, cancel, e := requestContext(ctx, p)
	if e != nil {
		return ErrForbidden
	}
	defer cancel()
	b := budgetFrom(ctx)
	conn, e := s.pool.Acquire(ctx)
	if e != nil {
		return publicError(e)
	}
	var tx pgx.Tx
	defer func() { cleanup(conn, tx, false, 0, b) }()
	tx, e = begin(ctx, conn, pgx.ReadCommitted, b)
	if e != nil {
		return publicError(e)
	}
	m := newMeteredTx(tx, b)
	if _, e = m.Exec(ctx, "SET LOCAL lock_timeout='1s'"); e != nil {
		return publicError(e)
	}
	return publicError(access.AuthorizeImport(ctx, m, s.schema, p, time.Now()))
}
func commit(ctx context.Context, tx pgx.Tx, b *sqlBudget) error {
	sub, cancel, e := directContext(ctx, b, controlSQL)
	if e != nil {
		return publicError(e)
	}
	defer cancel()
	e = tx.Commit(sub)
	if e == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(e, &pe) {
		return publicError(e)
	}
	if errors.Is(e, pgx.ErrTxCommitRollback) {
		return ErrRetryable
	}
	return ErrCommitUnknown
}
func (s *Service) Apply(ctx context.Context, pn access.ImportPrincipal, requestID string, raw []byte) (Result, error) {
	fail := func(e error) (Result, error) { return Result{}, publicError(e) }
	pn = principal(pn)
	ctx, cancel, e := requestContext(ctx, pn)
	if e != nil {
		return fail(ErrForbidden)
	}
	defer cancel()
	if !uuidPattern.MatchString(requestID) {
		return fail(ErrInvalidInput)
	}
	requestID = strings.ToLower(requestID)
	if e = s.Preauthorize(ctx, pn); e != nil {
		return fail(e)
	}
	if len(raw) > p.MaxInput {
		return fail(ErrInvalidInput)
	}
	_, doc := p.EvaluateDocument(ctx, raw)
	if doc == nil {
		return fail(ErrInvalidInput)
	}
	tenants := doc.Tables["tenants"]
	if len(tenants) != 1 {
		return fail(ErrInvalidInput)
	}
	if tenants[0].Values["id"].Text != pn.Identity.TenantID {
		return fail(ErrForbidden)
	}
	bind := BatchBinding{TenantID: pn.Identity.TenantID, RequestID: requestID, ActorUserID: pn.Identity.UserID, ActingMembershipID: pn.Identity.ActingMembershipID, ProtocolVersion: protocolVersion, InputSHA256: sha256.Sum256(raw)}
	b := budgetFrom(ctx)
	conn, e := s.pool.Acquire(ctx)
	if e != nil {
		return fail(e)
	}
	var tx pgx.Tx
	locked := false
	key := batchLockKey(bind.TenantID, requestID)
	defer func() { cleanup(conn, tx, locked, key, b) }()
	sub, stop, e := directContext(ctx, b, controlSQL)
	if e != nil {
		return fail(e)
	}
	e = conn.QueryRow(sub, "SELECT pg_catalog.pg_try_advisory_lock($1)", key).Scan(&locked)
	stop()
	if e != nil {
		return fail(e)
	}
	if !locked {
		return fail(ErrBusy)
	}
	tx, e = begin(ctx, conn, pgx.Serializable, b)
	if e != nil {
		return fail(e)
	}
	m := newMeteredTx(tx, b)
	if _, e = m.Exec(ctx, "SET LOCAL lock_timeout='1s'"); e != nil {
		return fail(e)
	}
	if e = access.AuthorizeImport(ctx, m, s.schema, pn, time.Now()); e != nil {
		return fail(e)
	}
	stored, found, e := lookupReceipt(ctx, m, s.schema, bind.TenantID, requestID)
	if e != nil {
		return fail(e)
	}
	if found {
		if !MatchBinding(bind, stored.Binding) {
			return fail(ErrKeyConflict)
		}
		encoded, e := EncodeReceipt(stored.Receipt)
		if e != nil {
			return fail(e)
		}
		return Result{stored.Receipt, encoded, true}, nil
	}
	snapshot, e := c.ReadAppendSnapshot(ctx, m, s.schema, bind.TenantID, *doc)
	if e != nil {
		return fail(e)
	}
	decisions, issues, e := c.Decide(ctx, *doc, snapshot)
	if e != nil {
		return fail(e)
	}
	plan, e := BuildPlan(ctx, *doc, decisions, issues)
	if e != nil {
		return fail(e)
	}
	r := Receipt{ProtocolVersion: protocolVersion, State: Applied, Reason: None, Counts: plan.Counts, Issues: plan.Issues, ErrorsTotal: plan.ErrorsTotal, IssuesTruncated: plan.IssuesTruncated}
	if r.Counts["total"].Conflict > 0 {
		r.State = Rejected
		r.Reason = InputConflict
	} else {
		if _, e = m.Exec(ctx, "SET LOCAL search_path=pg_catalog,"+pgx.Identifier{s.schema}.Sanitize()); e != nil {
			return fail(e)
		}
		if _, e = m.Exec(ctx, "SAVEPOINT import_business"); e != nil {
			return fail(e)
		}
		e = insertPlan(ctx, m, s.schema, plan)
		if e != nil {
			if !constraintError(e) {
				return fail(e)
			}
			if _, e = m.Exec(ctx, "ROLLBACK TO SAVEPOINT import_business"); e != nil {
				return fail(e)
			}
			r.State = Rejected
			r.Reason = DatabaseConstraintConflict
		} else {
			for name, v := range r.Counts {
				v.Inserted = v.New
				r.Counts[name] = v
			}
		}
		if _, e = m.Exec(ctx, "RELEASE SAVEPOINT import_business"); e != nil {
			return fail(e)
		}
	}
	r.CompletedAt = time.Now().UTC().Truncate(time.Microsecond)
	encoded, e := EncodeReceipt(r)
	if e != nil {
		return fail(e)
	}
	if e = insertReceipt(ctx, m, s.schema, StoredBatch{bind, r}); e != nil {
		return fail(e)
	}
	if e = access.AuditImportTerminal(ctx, m, s.schema, pn, requestID, string(r.State), string(r.Reason), r.CompletedAt); e != nil {
		return fail(ErrAuditUnavailable)
	}
	if e = access.AuthorizeImport(ctx, m, s.schema, pn, time.Now()); e != nil {
		return fail(e)
	}
	if e = commit(ctx, tx, b); e != nil {
		return fail(e)
	}
	return Result{r, encoded, false}, nil
}
