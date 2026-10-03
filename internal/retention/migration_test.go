package retention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

const (
	tenantA        = "00000000-0000-4000-8000-000000001001"
	tenantB        = "00000000-0000-4000-8000-000000001002"
	userA          = "00000000-0000-4000-8000-000000001010"
	userA2         = "00000000-0000-4000-8000-000000001011"
	userA3         = "00000000-0000-4000-8000-000000001012"
	memberA        = "00000000-0000-4000-8000-000000001020"
	memberA2       = "00000000-0000-4000-8000-000000001021"
	memberA3       = "00000000-0000-4000-8000-000000001022"
	conversationA  = "00000000-0000-4000-8000-000000001030"
	conversationA2 = "00000000-0000-4000-8000-000000001031"
	conversationB  = "00000000-0000-4000-8000-000000001032"
)

var fixedTime = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IM_TEST_DATABASE_URL for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("im_retention_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	if _, err := admin.Exec(ctx, "SET search_path TO "+schema+",public"); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob("../../db/migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.PgConn().Exec(ctx, string(data)).ReadAll(); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	exec(t, pool, `INSERT INTO tenants(id,code,name) VALUES ($1,'a','A'),($2,'b','B')`, tenantA, tenantB)
	exec(t, pool, `INSERT INTO legal_entities(id,tenant_id,code,name) VALUES
 ('00000000-0000-4000-8000-000000001003',$1,'l','L'),('00000000-0000-4000-8000-000000001004',$2,'l','L')`, tenantA, tenantB)
	exec(t, pool, `INSERT INTO organizations(id,tenant_id,legal_entity_id,org_type,code,name) VALUES
 ('00000000-0000-4000-8000-000000001005',$1,'00000000-0000-4000-8000-000000001003','company','o','O'),
 ('00000000-0000-4000-8000-000000001006',$2,'00000000-0000-4000-8000-000000001004','company','o','O')`, tenantA, tenantB)
	exec(t, pool, `INSERT INTO users(id,tenant_id,global_employee_no,display_name) VALUES
 ($1,$4,'1','A'),($2,$4,'2','B'),($3,$4,'3','C'),
 ('00000000-0000-4000-8000-000000001013',$5,'1','D'),('00000000-0000-4000-8000-000000001014',$5,'2','E')`, userA, userA2, userA3, tenantA, tenantB)
	exec(t, pool, `INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES
 ($1,$4,$5,'00000000-0000-4000-8000-000000001005','2000-01-01'),
 ($2,$4,$6,'00000000-0000-4000-8000-000000001005','2000-01-01'),
 ($3,$4,$7,'00000000-0000-4000-8000-000000001005','2000-01-01'),
 ('00000000-0000-4000-8000-000000001023',$8,'00000000-0000-4000-8000-000000001013','00000000-0000-4000-8000-000000001006','2000-01-01'),
 ('00000000-0000-4000-8000-000000001024',$8,'00000000-0000-4000-8000-000000001014','00000000-0000-4000-8000-000000001006','2000-01-01')`, memberA, memberA2, memberA3, tenantA, userA, userA2, userA3, tenantB)
	exec(t, pool, `INSERT INTO conversations(id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id) VALUES
 ($1,$4,$5,$6,$8,$9,$5),($2,$4,$5,$7,$8,$10,$5),
 ($3,$11,'00000000-0000-4000-8000-000000001013','00000000-0000-4000-8000-000000001014','00000000-0000-4000-8000-000000001023','00000000-0000-4000-8000-000000001024','00000000-0000-4000-8000-000000001013')`, conversationA, conversationA2, conversationB, tenantA, userA, userA2, userA3, memberA, memberA2, memberA3, tenantB)
	exec(t, pool, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from)
 VALUES ('00000000-0000-4000-8000-000000001040',$1,$2,'00000000-0000-4000-8000-000000001005','group_admin','2000-01-01')`, tenantA, memberA)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func seedMessage(t *testing.T, pool *pgxpool.Pool, conversation string, seq int64, acceptedAt time.Time) {
	t.Helper()
	tenant, user, member := tenantA, userA, memberA
	if conversation == conversationB {
		tenant = tenantB
		user = "00000000-0000-4000-8000-000000001013"
		member = "00000000-0000-4000-8000-000000001023"
	}
	exec(t, pool, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at)
 VALUES ($1,$2,$3,$4,$5,$6,'private body',decode(repeat('ab',32),'hex'),$7)`, tenant, conversation, seq, user, member, fmt.Sprintf("0199f04a-0000-7000-8000-%012x", seq), acceptedAt)
	exec(t, pool, "UPDATE conversations SET last_seq=GREATEST(last_seq,$2) WHERE id=$1", conversation, seq)
}

func setDays(t *testing.T, pool *pgxpool.Pool, tenant string, days int) {
	t.Helper()
	user := userA
	if tenant == tenantB {
		user = "00000000-0000-4000-8000-000000001013"
	}
	exec(t, pool, `UPDATE tenants SET message_body_retention_days=$2,retention_version=retention_version+1,
 retention_approval_reference='TEST',retention_approved_by_user_id=$3,retention_approved_at=$4 WHERE id=$1`, tenant, days, user, fixedTime)
}

func identity() access.TrustedIdentity {
	return access.TrustedIdentity{TenantID: tenantA, UserID: userA, ActingMembershipID: memberA}
}

func testWorker(pool *pgxpool.Pool, size int) Worker {
	return Worker{DB: pool, BatchSize: size, clock: func(context.Context, pgx.Tx) (time.Time, error) { return fixedTime, nil }}
}

func assertCounts(t *testing.T, pool *pgxpool.Pool, cleared, batches int) {
	t.Helper()
	var actualCleared, actualBatches int
	err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM messages WHERE body_cleared_at IS NOT NULL),(SELECT count(*) FROM message_body_clear_batches)`).Scan(&actualCleared, &actualBatches)
	if err != nil || actualCleared != cleared || actualBatches != batches {
		t.Fatalf("counts=%d/%d want=%d/%d err=%v", actualCleared, actualBatches, cleared, batches, err)
	}
}
