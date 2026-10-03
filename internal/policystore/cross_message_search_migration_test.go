package policystore_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestCrossMessageSearchIndexesUpDown(t *testing.T) {
	conn := db(t)
	seedDirectConversation(t, conn)
	if _, e := (policystore.Service{DB: conn, Now: func() time.Time { return at }}).SendTextMessage(context.Background(), publisher(), directA, clientUUIDv7(at, 1740), "migration保留正文"); e != nil {
		t.Fatal(e)
	}
	indexes := []string{"conversations_direct_low_search", "conversations_direct_high_search"}
	check := func(want bool) {
		t.Helper()
		for _, name := range indexes {
			var def string
			err := conn.QueryRow(context.Background(), `SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1`, name).Scan(&def)
			if want {
				if err != nil || !strings.Contains(def, "tenant_id, direct_user_") || !strings.Contains(def, "_id, id)") || !strings.Contains(def, "kind = 'direct'") {
					t.Fatalf("index %s: %s %v", name, def, err)
				}
			} else if err == nil {
				t.Fatalf("index retained %s", name)
			}
		}
	}
	check(true)
	for _, direction := range []string{"down", "up"} {
		sql, err := os.ReadFile("../../db/migrations/000017_cross_message_search_indexes." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.PgConn().Exec(context.Background(), string(sql)).ReadAll(); err != nil {
			t.Fatal(err)
		}
		check(direction == "up")
		var count int
		if e := conn.QueryRow(context.Background(), "SELECT count(*) FROM messages WHERE conversation_id=$1 AND text_body=$2", directA, "migration保留正文").Scan(&count); e != nil || count != 1 {
			t.Fatal("migration lost message", count, e)
		}
	}
	var n int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE id=$1", directA).Scan(&n); err != nil || n != 1 {
		t.Fatalf("conversation lost %d %v", n, err)
	}
}
