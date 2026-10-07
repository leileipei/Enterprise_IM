package importapply

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync"
	"testing"
	"time"
)

func TestAppendPGAuditZeroRows(t *testing.T) {
	for _, mode := range []string{"apply", "get"} {
		t.Run(mode, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			s, _ := NewService(f.Pool, f.Schema)
			ctx := context.Background()
			if mode == "get" {
				if _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); e != nil {
					t.Fatal(e)
				}
			}
			before := databaseCounts(t, f)
			if _, e := f.Admin.Exec(ctx, "CREATE FUNCTION suppress_import_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;CREATE TRIGGER suppress_import_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION suppress_import_audit()"); e != nil {
				t.Fatal(e)
			}
			var e error
			if mode == "get" {
				_, e = s.Get(ctx, actor, fixtureRequest)
			} else {
				_, e = s.Apply(ctx, actor, fixtureRequest, applyInput(t))
			}
			if !errors.Is(e, ErrAuditUnavailable) {
				t.Fatal("zero-row audit accepted", e)
			}
			if got := databaseCounts(t, f); got != before {
				t.Fatal("missing audit partially committed", got)
			}
			var queries int
			if e = f.Pool.QueryRow(ctx, "SELECT count(*) FROM "+tableName(f.Schema, "audit_events")+" WHERE action='controlled_import.query'").Scan(&queries); e != nil || queries != 0 {
				t.Fatal("missing query audit treated as successful")
			}
		})
	}
}

type endPreauthorize struct {
	cancel   context.CancelFunc
	deadline time.Time
	once     sync.Once
}
type rollbackKey struct{}

func (p *endPreauthorize) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, rollbackKey{}, d.SQL == "rollback")
}
func (p *endPreauthorize) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if yes, _ := ctx.Value(rollbackKey{}).(bool); yes && d.Err == nil {
		p.once.Do(func() {
			if !p.deadline.IsZero() {
				if wait := time.Until(p.deadline.Add(10 * time.Millisecond)); wait > 0 {
					time.Sleep(wait)
				}
			} else {
				p.cancel()
			}
		})
	}
}
func TestAppendPGPreflightCanceled(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			before := databaseCounts(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tracer := &endPreauthorize{cancel: cancel}
			if mode == "deadline" {
				tracer.deadline = time.Now().Add(400 * time.Millisecond)
				ctx, cancel = context.WithDeadline(context.Background(), tracer.deadline)
				defer cancel()
			}
			cfg := f.Pool.Config()
			cfg.ConnConfig.Tracer = tracer
			pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer pool.Close()
			s, _ := NewService(pool, f.Schema)
			_, e = s.Apply(ctx, actor, fixtureRequest, applyInput(t))
			if !errors.Is(e, ErrRetryable) {
				t.Fatal("interrupted preflight reported input error", e)
			}
			if got := databaseCounts(t, f); got != before {
				t.Fatal("interrupted preflight left rows", got)
			}
		})
	}
}
func TestAppendPGTerminalAndTriggerFault(t *testing.T) {
	for _, mode := range []string{"receipt", "unknownTrigger"} {
		t.Run(mode, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			s, _ := NewService(f.Pool, f.Schema)
			ctx := context.Background()
			before := databaseCounts(t, f)
			sql := "ALTER TABLE import_batches ADD CHECK(protocol_version<>'controlled_append_v1')"
			if mode == "unknownTrigger" {
				sql = "CREATE FUNCTION fail_import_department() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private synthetic marker' USING ERRCODE='P0001'; END $$;CREATE TRIGGER fail_import_department BEFORE INSERT ON user_departments FOR EACH ROW EXECUTE FUNCTION fail_import_department()"
			}
			if _, e := f.Admin.Exec(ctx, sql); e != nil {
				t.Fatal(e)
			}
			result, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t))
			if !errors.Is(e, ErrDatabaseUnavailable) || len(result.Encoded) != 0 {
				t.Fatal("terminal fault produced misleading result", e)
			}
			if got := databaseCounts(t, f); got != before {
				t.Fatal("terminal fault leaked rows", got)
			}
		})
	}
}
