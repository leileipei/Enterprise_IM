package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
)

const (
	tenantID       = "00000000-0000-4000-8000-000000000901"
	legalID        = "00000000-0000-4000-8000-000000000902"
	orgID          = "00000000-0000-4000-8000-000000000903"
	userLow        = "00000000-0000-4000-8000-000000000904"
	userHigh       = "00000000-0000-4000-8000-000000000905"
	memberLow      = "00000000-0000-4000-8000-000000000906"
	memberHigh     = "00000000-0000-4000-8000-000000000907"
	conversationID = "00000000-0000-4000-8000-000000000908"
	messageID      = "00000000-0000-4000-8000-000000000909"
	eventID        = "00000000-0000-4000-8000-000000000910"
)

var fixedNow = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("IM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IM_TEST_DATABASE_URL for PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("im_outbox_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000001_group_foundation", "000002_admin_access", "000003_policy_store", "000005_direct_conversations", "000006_message_write"} {
		data, err := os.ReadFile("../../db/migrations/" + name + ".up.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.PgConn().Exec(ctx, string(data)).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	return pool
}

func seedEvent(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tenants (id,code,name) VALUES ($1,'g','Group')`, []any{tenantID}},
		{`INSERT INTO legal_entities (id,tenant_id,code,name) VALUES ($1,$2,'l','Legal')`, []any{legalID, tenantID}},
		{`INSERT INTO organizations (id,tenant_id,legal_entity_id,org_type,code,name) VALUES ($1,$2,$3,'company','o','Org')`, []any{orgID, tenantID, legalID}},
		{`INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'L','Low'),($3,$2,'H','High')`, []any{userLow, tenantID, userHigh}},
		{`INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01'),($5,$2,$6,$4,'2020-01-01')`, []any{memberLow, tenantID, userLow, orgID, memberHigh, userHigh}},
		{`INSERT INTO conversations (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id) VALUES ($1,$2,$3,$4,$5,$6,$3)`, []any{conversationID, tenantID, userLow, userHigh, memberLow, memberHigh}},
		{`INSERT INTO messages (id,tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at) VALUES ($1,$2,$3,1,$4,$5,'0199f04a-0000-7000-8000-000000000001','private body',decode(repeat('ab',32),'hex'),$6)`, []any{messageID, tenantID, conversationID, userLow, memberLow, fixedNow}},
		{`INSERT INTO outbox_events (id,tenant_id,conversation_id,seq,message_id,event_type,next_retry_at,created_at) VALUES ($1,$2,$3,1,$4,'message_created',$5,$5)`, []any{eventID, tenantID, conversationID, messageID, fixedNow}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

type publisherFunc func(context.Context, outbox.Event) error

func (f publisherFunc) Publish(ctx context.Context, event outbox.Event) error { return f(ctx, event) }

func TestProcessOnePublishesAndMarksEvent(t *testing.T) {
	db := database(t)
	seedEvent(t, db)
	var got []outbox.Event
	worker := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow }, Publisher: publisherFunc(func(_ context.Context, event outbox.Event) error {
		got = append(got, event)
		return nil
	})}
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed || len(got) != 1 || got[0].ID != eventID || got[0].TenantID != tenantID || got[0].ConversationID != conversationID || got[0].MessageID != messageID || got[0].Seq != 1 || got[0].EventType != "message_created" {
		t.Fatalf("published event: processed=%t event=%+v err=%v", processed, got, err)
	}
	processed, err = worker.ProcessOne(context.Background())
	if err != nil || processed || len(got) != 1 {
		t.Fatalf("republished event: %t %d %v", processed, len(got), err)
	}
	var status string
	var attempts int
	var publishedAt time.Time
	if err := db.QueryRow(context.Background(), "SELECT status,attempt_count,published_at FROM outbox_events WHERE tenant_id=$1 AND id=$2", tenantID, eventID).Scan(&status, &attempts, &publishedAt); err != nil || status != "published" || attempts != 1 || !publishedAt.Equal(fixedNow) {
		t.Fatalf("outbox row: %q %d %v %v", status, attempts, publishedAt, err)
	}
}

func TestProcessOneFailureSchedulesRetryAndRecovers(t *testing.T) {
	db := database(t)
	seedEvent(t, db)
	now := fixedNow
	calls := 0
	worker := outbox.Worker{DB: db, Now: func() time.Time { return now }, Publisher: publisherFunc(func(context.Context, outbox.Event) error {
		calls++
		if calls == 1 {
			return errors.New("redis unavailable")
		}
		return nil
	})}
	processed, err := worker.ProcessOne(context.Background())
	if !processed || err == nil {
		t.Fatalf("failure not reported: %t %v", processed, err)
	}
	var status string
	var attempts int
	var retryAt time.Time
	if err := db.QueryRow(context.Background(), "SELECT status,attempt_count,next_retry_at FROM outbox_events WHERE tenant_id=$1 AND id=$2", tenantID, eventID).Scan(&status, &attempts, &retryAt); err != nil || status != "pending" || attempts != 1 || !retryAt.Equal(fixedNow.Add(time.Second)) {
		t.Fatalf("retry row: %q %d %v %v", status, attempts, retryAt, err)
	}
	processed, err = worker.ProcessOne(context.Background())
	if processed || err != nil || calls != 1 {
		t.Fatalf("early retry: %t %d %v", processed, calls, err)
	}
	now = fixedNow.Add(time.Second)
	processed, err = worker.ProcessOne(context.Background())
	if !processed || err != nil || calls != 2 {
		t.Fatalf("due retry: %t %d %v", processed, calls, err)
	}
}

func TestProcessOneConcurrentWorkersSkipLocked(t *testing.T) {
	db := database(t)
	seedEvent(t, db)
	entered := make(chan struct{})
	release := make(chan struct{})
	worker := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow }, Publisher: publisherFunc(func(context.Context, outbox.Event) error {
		close(entered)
		<-release
		return nil
	})}
	var wg sync.WaitGroup
	wg.Add(1)
	var firstProcessed bool
	var firstErr error
	go func() { defer wg.Done(); firstProcessed, firstErr = worker.ProcessOne(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first worker never claimed event")
	}
	second := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow }, Publisher: publisherFunc(func(context.Context, outbox.Event) error { t.Fatal("second worker published locked event"); return nil })}
	secondProcessed, secondErr := second.ProcessOne(context.Background())
	close(release)
	wg.Wait()
	if !firstProcessed || firstErr != nil || secondProcessed || secondErr != nil {
		t.Fatalf("concurrent claim: first=%t %v second=%t %v", firstProcessed, firstErr, secondProcessed, secondErr)
	}
}

func TestProcessOneStatusUpdateFailureKeepsEventPending(t *testing.T) {
	db := database(t)
	seedEvent(t, db)
	if _, err := db.Exec(context.Background(), `CREATE FUNCTION fail_publish_status() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'status unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), "CREATE TRIGGER fail_publish_status BEFORE UPDATE ON outbox_events FOR EACH ROW WHEN (NEW.status = 'published') EXECUTE FUNCTION fail_publish_status()"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow }, Publisher: publisherFunc(func(context.Context, outbox.Event) error { calls++; return nil })}
	processed, err := worker.ProcessOne(context.Background())
	if !processed || err == nil || calls != 1 {
		t.Fatalf("status failure not reported: %t %d %v", processed, calls, err)
	}
	var status string
	var attempts int
	if err := db.QueryRow(context.Background(), "SELECT status,attempt_count FROM outbox_events WHERE tenant_id=$1 AND id=$2", tenantID, eventID).Scan(&status, &attempts); err != nil || status != "pending" || attempts != 0 {
		t.Fatalf("status failure changed row: %q %d %v", status, attempts, err)
	}
}

func TestProcessOneRejectsSuppressedStatusUpdate(t *testing.T) {
	db := database(t)
	seedEvent(t, db)
	if _, err := db.Exec(context.Background(), `CREATE FUNCTION skip_publish_status() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), "CREATE TRIGGER skip_publish_status BEFORE UPDATE ON outbox_events FOR EACH ROW WHEN (NEW.status = 'published') EXECUTE FUNCTION skip_publish_status()"); err != nil {
		t.Fatal(err)
	}
	worker := outbox.Worker{DB: db, Now: func() time.Time { return fixedNow }, Publisher: publisherFunc(func(context.Context, outbox.Event) error { return nil })}
	processed, err := worker.ProcessOne(context.Background())
	if !processed || err == nil {
		t.Fatalf("suppressed update reported success: %t %v", processed, err)
	}
	var status string
	if err := db.QueryRow(context.Background(), "SELECT status FROM outbox_events WHERE tenant_id=$1 AND id=$2", tenantID, eventID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("suppressed update changed row: %s %v", status, err)
	}
}
