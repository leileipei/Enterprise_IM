package importapply

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"sync"
	"testing"
	"time"
)

type pauseQuery struct {
	match            string
	after            bool
	entered, release chan struct{}
	once             sync.Once
	mu               sync.Mutex
	codes            []string
}
type pauseKey struct{}

func (p *pauseQuery) wait(ctx context.Context) {
	p.once.Do(func() {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	})
}
func (p *pauseQuery) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	hit := strings.Contains(d.SQL, p.match)
	if hit && !p.after {
		p.wait(ctx)
	}
	return context.WithValue(ctx, pauseKey{}, hit)
}
func (p *pauseQuery) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	var pe *pgconn.PgError
	if errors.As(d.Err, &pe) {
		p.mu.Lock()
		p.codes = append(p.codes, pe.Code)
		p.mu.Unlock()
	}
	if hit, _ := ctx.Value(pauseKey{}).(bool); hit && p.after {
		p.wait(ctx)
	}
}
func pausedService(t *testing.T, f *appendFixture, match string, after bool) (*Service, *pauseQuery) {
	t.Helper()
	cfg := f.Pool.Config()
	p := &pauseQuery{match: match, after: after, entered: make(chan struct{}), release: make(chan struct{})}
	cfg.ConnConfig.Tracer = p
	pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	s, e := NewService(pool, f.Schema)
	if e != nil {
		t.Fatal(e)
	}
	return s, p
}
func awaitPause(t *testing.T, p *pauseQuery) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("query boundary was not reached")
	}
}
func TestAppendPGConcurrentSameBatch(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, pause := pausedService(t, f, "SET LOCAL search_path=pg_catalog", false)
	other, _ := NewService(f.Pool, f.Schema)
	done := make(chan error, 1)
	go func() { _, e := s.Apply(context.Background(), actor, fixtureRequest, applyInput(t)); done <- e }()
	awaitPause(t, pause)
	if _, e := other.Apply(context.Background(), actor, fixtureRequest, applyInput(t)); !errors.Is(e, ErrBusy) {
		t.Fatal("second instance did not observe active batch", e)
	}
	if _, e := other.Get(context.Background(), actor, fixtureRequest); !errors.Is(e, ErrBusy) {
		t.Fatal("GET did not observe same coordination lock", e)
	}
	close(pause.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	r, e := other.Apply(context.Background(), actor, fixtureRequest, applyInput(t))
	if e != nil || !r.Replay {
		t.Fatal("post-completion retry", e)
	}
	if c := databaseCounts(t, f); c[6] != 1 || c[7] != 1 || c[3] != 2 {
		t.Fatal("duplicate batch", c)
	}
}
func TestAppendPGSnapshotAfterSessionLock(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	s, pause := pausedService(t, f, "SELECT pg_catalog.pg_try_advisory_lock", true)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); done <- e }()
	awaitPause(t, pause)
	// Commit an independent terminal after the session lock but before importer BEGIN.
	tx, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	r := receiptFixture()
	r.CompletedAt = time.Now().UTC().Truncate(time.Microsecond)
	bind := BatchBinding{TenantID: fixtureTenant, RequestID: fixtureRequest, ActorUserID: fixtureActor, ActingMembershipID: fixtureMembership, ProtocolVersion: protocolVersion, InputSHA256: shaInput(applyInput(t))}
	if e = insertReceipt(ctx, tx, f.Schema, StoredBatch{bind, r}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	close(pause.release)
	if e = <-done; e != nil {
		t.Fatal("BEGIN used snapshot from lock statement", e)
	}
	if c := databaseCounts(t, f); c[6] != 1 || c[7] != 0 || c[3] != 1 {
		t.Fatal("fresh terminal was not replayed", c)
	}
}
func TestAppendPGConcurrentMutation(t *testing.T) {
	for _, scenario := range []string{"mapping", "grant", "membership", "organization", "globalUUID", "naturalKey"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			ctx := context.Background()
			s, pause := pausedService(t, f, "SELECT pg_catalog.pg_try_advisory_lock", true)
			done := make(chan error, 1)
			go func() { _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); done <- e }()
			awaitPause(t, pause)
			var sql string
			switch scenario {
			case "mapping":
				sql = "UPDATE external_identities SET status='disabled'"
			case "grant":
				sql = "UPDATE admin_grants SET status='revoked'"
			case "membership":
				sql = "UPDATE user_organizations SET effective_to=now()"
			case "organization":
				sql = "UPDATE organizations SET status='disabled'"
			case "globalUUID":
				sql = "INSERT INTO tenants(id,code,name) VALUES('99100000-0000-4000-8000-000000000001','other','Other');INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('96000000-0000-4000-8000-000000000004','99100000-0000-4000-8000-000000000001','other','Other')"
			case "naturalKey":
				sql = "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('99100000-0000-4000-8000-000000000002','" + fixtureTenant + "','newperson','Other')"
			}
			if _, e := f.Admin.Exec(ctx, sql); e != nil {
				t.Fatal(e)
			}
			close(pause.release)
			e := <-done
			if scenario == "globalUUID" || scenario == "naturalKey" {
				if e != nil {
					t.Fatal(e)
				}
				var state string
				if e = f.Pool.QueryRow(ctx, "SELECT state FROM import_batches").Scan(&state); e != nil || state != "rejected" {
					t.Fatal("raced conflict accepted", e)
				}
			} else {
				if !errors.Is(e, ErrForbidden) {
					t.Fatal("authority change cached", e)
				}
				if c := databaseCounts(t, f); c[6] != 0 || c[7] != 0 {
					t.Fatal("unauthorized terminal")
				}
			}
		})
	}
}
func shaInput(b []byte) [32]byte { return sha256.Sum256(b) }
func TestAppendPGRetryErrors(t *testing.T) {
	for _, scenario := range []string{"serialization", "lockTimeout", "deadlock"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			ctx := context.Background()
			plain, _ := NewService(f.Pool, f.Schema)
			if _, e := plain.Apply(ctx, actor, "99200000-0000-4000-8000-000000000001", applyInput(t)); e != nil {
				t.Fatal(e)
			}
			before := databaseCounts(t, f)
			match := "SET LOCAL search_path=pg_catalog"
			if scenario == "deadlock" {
				match = "FROM " + tableName(f.Schema, "users") + " WHERE"
			}
			s, pause := pausedService(t, f, match, false)
			defer func() {
				expected := map[string]string{"serialization": "40001", "lockTimeout": "55P03", "deadlock": "40P01"}[scenario]
				pause.mu.Lock()
				defer pause.mu.Unlock()
				for _, code := range pause.codes {
					if code == expected {
						t.Log("actual_SQLSTATE_" + code)
						return
					}
				}
				t.Errorf("expected actual SQLSTATE %s, got %v", expected, pause.codes)
			}()
			if scenario == "deadlock" {
				role := pgx.Identifier{f.Pool.Config().ConnConfig.User}.Sanitize()
				if _, e := f.Admin.Exec(ctx, "GRANT SET ON PARAMETER deadlock_timeout TO "+role); e != nil {
					t.Fatal(e)
				}
				defer f.Admin.Exec(ctx, "REVOKE SET ON PARAMETER deadlock_timeout FROM "+role)

				conn, e := s.pool.Acquire(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = conn.Exec(ctx, "SET deadlock_timeout='10ms'"); e != nil {
					t.Fatal(e)
				}
				conn.Release()
			}
			done := make(chan error, 1)
			go func() { _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); done <- e }()
			awaitPause(t, pause)
			if scenario == "serialization" {
				if _, e := f.Admin.Exec(ctx, "UPDATE users SET display_name='Concurrent' WHERE id='96000000-0000-4000-8000-000000000004'"); e != nil {
					t.Fatal(e)
				}
			} else {
				tx, e := f.Admin.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = tx.Exec(ctx, "SET LOCAL deadlock_timeout='5s'"); e != nil {
					t.Fatal(e)
				}
				if _, e = tx.Exec(ctx, "UPDATE users SET display_name='Concurrent' WHERE id='96000000-0000-4000-8000-000000000004'"); e != nil {
					t.Fatal(e)
				}
				if scenario == "deadlock" {
					blocked := make(chan error, 1)
					go func() {
						_, e := tx.Exec(ctx, "UPDATE organizations SET name='Concurrent' WHERE id='96000000-0000-4000-8000-000000000002'")
						blocked <- e
					}()
					waitDatabaseBlock(t, f)
					close(pause.release)
					e = <-done
					if !errors.Is(e, ErrRetryable) {
						t.Fatal("real deadlock not retryable", e)
					}
					<-blocked
				} else {
					close(pause.release)
					e = <-done
					if !errors.Is(e, ErrRetryable) {
						t.Fatal("real lock timeout not retryable", e)
					}
				}
				tx.Rollback(ctx)
				if c := databaseCounts(t, f); c[6] != before[6] || c[7] != before[7] {
					t.Fatal("retry error persisted terminal")
				}
				return
			}
			close(pause.release)
			e := <-done
			if !errors.Is(e, ErrRetryable) {
				t.Fatal("real serialization error not retryable", e)
			}
			if c := databaseCounts(t, f); c[6] != before[6] || c[7] != before[7] {
				t.Fatal("serialization persisted terminal")
			}
		})
	}
}
func waitDatabaseBlock(t *testing.T, f *appendFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		var n int
		e := f.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_locks WHERE NOT granted AND locktype='transactionid'").Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected real database blocking was not reached")
}

func TestAppendPGConcurrentInsertRace(t *testing.T) {
	for _, scenario := range []string{"globalUUID", "naturalKey"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			ctx := context.Background()
			s, pause := pausedService(t, f, "INSERT INTO "+tableName(f.Schema, "legal_entities"), false)
			done := make(chan error, 1)
			go func() { _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); done <- e }()
			awaitPause(t, pause)
			sql := "INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('99800000-0000-4000-8000-000000000001','" + fixtureTenant + "','newperson','Racing')"
			if scenario == "globalUUID" {
				sql = "INSERT INTO tenants(id,code,name) VALUES('99800000-0000-4000-8000-000000000002','race','Race');INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES('96000000-0000-4000-8000-000000000004','99800000-0000-4000-8000-000000000002','race','Racing')"
			}
			if _, e := f.Admin.Exec(ctx, sql); e != nil {
				t.Fatal(e)
			}
			close(pause.release)
			e := <-done
			if e != nil && !errors.Is(e, ErrRetryable) {
				t.Fatal("unsafe race error", e)
			}
			if c := databaseCounts(t, f); c[0] != 1 || c[1] != 1 || c[2] != 0 || c[5] != 0 {
				t.Fatal("race left partial business writes", c)
			}
			if e == nil {
				var raw []byte
				if e = f.Pool.QueryRow(ctx, "SELECT receipt FROM import_batches").Scan(&raw); e != nil {
					t.Fatal(e)
				}
				r, e := DecodeReceipt(raw)
				if e != nil || r.State != Rejected || r.Reason != DatabaseConstraintConflict || r.Counts["total"].Inserted != 0 {
					t.Fatal("race not rejected", e)
				}
			}
		})
	}
}
func TestAppendPGAuthorityExpiresDuringWrite(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	ctx := context.Background()
	end := time.Now().Add(time.Second)
	if _, e := f.Admin.Exec(ctx, "UPDATE admin_grants SET effective_to=$1", end); e != nil {
		t.Fatal(e)
	}
	before := databaseCounts(t, f)
	s, pause := pausedService(t, f, "INSERT INTO "+tableName(f.Schema, "import_batches"), false)
	done := make(chan error, 1)
	go func() { _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); done <- e }()
	awaitPause(t, pause)
	if delay := time.Until(end.Add(20 * time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	close(pause.release)
	if e := <-done; !errors.Is(e, ErrForbidden) {
		t.Fatal("authority time cached until commit", e)
	}
	if c := databaseCounts(t, f); c != before {
		t.Fatal("expired grant partially committed", c)
	}
}
func TestAppendPGInventoryMutationBlocked(t *testing.T) {
	f := appendDB(t, 22)
	actor := seedApplyActor(t, f)
	ctx := context.Background()
	plain, _ := NewService(f.Pool, f.Schema)
	if _, e := plain.Apply(ctx, actor, "99900000-0000-4000-8000-000000000001", applyInput(t)); e != nil {
		t.Fatal(e)
	}
	s, pause := pausedService(t, f, "INSERT INTO "+tableName(f.Schema, "import_batches"), false)
	done := make(chan error, 1)
	go func() { _, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t)); done <- e }()
	awaitPause(t, pause)
	updated := make(chan error, 1)
	go func() {
		_, e := f.Admin.Exec(ctx, "UPDATE organizations SET parent_id='95000000-0000-4000-8000-000000000011' WHERE id='96000000-0000-4000-8000-000000000002'")
		updated <- e
	}()
	waitDatabaseBlock(t, f)
	select {
	case <-updated:
		t.Fatal("existing parent changed inside snapshot")
	default:
	}
	close(pause.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if e := <-updated; e != nil {
		t.Fatal(e)
	}
}
