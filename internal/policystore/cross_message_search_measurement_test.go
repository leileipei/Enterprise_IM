package policystore_test

import (
	"context"
	"testing"
	"time"
)

// A reproducible local query-plan measurement, not a production capacity gate.
func TestCrossMessageSearchLocalSQLMeasurement(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	ctx := context.Background()
	run(t, conn, `INSERT INTO users(id,tenant_id,global_employee_no,display_name) SELECT ('00000000-0000-4000-8000-'||lpad((100000+n)::text,12,'0'))::uuid,$1,'scale-'||n,'scale' FROM generate_series(1,1000)n`, tenantA)
	run(t, conn, `INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) SELECT ('00000000-0000-4000-8000-'||lpad((200000+n)::text,12,'0'))::uuid,$1,('00000000-0000-4000-8000-'||lpad((100000+n)::text,12,'0'))::uuid,$2,$3 FROM generate_series(1,1000)n`, tenantA, orgA, at.Add(-time.Hour))
	run(t, conn, `INSERT INTO conversations(id,tenant_id,kind,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id) SELECT ('00000000-0000-4000-8000-'||lpad((300000+n)::text,12,'0'))::uuid,$1,'direct',$2,('00000000-0000-4000-8000-'||lpad((100000+n)::text,12,'0'))::uuid,$3,('00000000-0000-4000-8000-'||lpad((200000+n)::text,12,'0'))::uuid,$2 FROM generate_series(1,1000)n`, tenantA, adminA, adminM)
	run(t, conn, `INSERT INTO messages(tenant_id,conversation_id,seq,sender_user_id,sender_membership_id,client_msg_id,text_body,content_digest,accepted_at) SELECT $1,('00000000-0000-4000-8000-'||lpad((300000+n)::text,12,'0'))::uuid,s,$2,$3,('00000000-0000-7000-8000-'||lpad((n*10+s)::text,12,'0'))::uuid,'scale正文',decode(repeat('00',32),'hex'),$4 FROM generate_series(1,1000)n CROSS JOIN generate_series(1,10)s`, tenantA, adminA, adminM, at)
	g := crossID(400001)
	seedCrossGroup(t, conn, g)
	run(t, conn, "UPDATE conversation_membership_intervals SET status='left',leave_seq=1,left_at=joined_at WHERE conversation_id=$1 AND user_id=$2", g, adminA)
	for n := 2; n <= 100; n++ {
		leave := int64(n)
		if err := insertInterval(conn, crossID(700000+n), tenantA, g, adminA, adminM, orgA, legalA, "owner", "left", int64(n), &leave); err != nil {
			t.Fatal(err)
		}
	}
	run(t, conn, "ANALYZE conversations; ANALYZE messages; ANALYZE conversation_membership_intervals")
	t.Log("scale: 1000 personal direct conversations, 10000 messages, 100 historical intervals for one user in one group")
	for _, q := range []struct {
		name, sql string
		args      []any
	}{
		{"direct candidates", `EXPLAIN (ANALYZE,BUFFERS) SELECT c.id::text FROM conversations c WHERE tenant_id=$1 AND kind='direct' AND direct_user_low_id=$2 AND c.id>$3::uuid ORDER BY c.id LIMIT 21`, []any{tenantA, adminA, crossID(300500)}},
		{"group candidates", `EXPLAIN (ANALYZE,BUFFERS) SELECT conversation_id FROM conversation_membership_intervals WHERE tenant_id=$1 AND user_id=$2 GROUP BY conversation_id ORDER BY conversation_id LIMIT 21`, []any{tenantA, adminA}},
		{"message keyset", `EXPLAIN (ANALYZE,BUFFERS) SELECT id,seq,text_body FROM messages WHERE tenant_id=$1 AND conversation_id=$2 AND seq>$3 ORDER BY seq LIMIT 501`, []any{tenantA, crossID(300500), 0}},
	} {
		rows, err := conn.Query(ctx, q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(q.name)
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			t.Log(line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
}
