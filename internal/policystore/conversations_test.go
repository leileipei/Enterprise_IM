package policystore_test

import (
	"context"
	"os"
	"testing"
)

const (
	directA = "00000000-0000-4000-8000-000000000401"
	directB = "00000000-0000-4000-8000-000000000402"
)

func TestDirectConversationSchemaEnforcesPairAndTenant(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	insert := `INSERT INTO conversations
 (id,tenant_id,kind,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,'direct',$3,$4,$5,$6,$7)`
	run(t, conn, insert, directA, tenantA, adminA, personA, adminM, targetM2, adminA)
	for _, tc := range []struct {
		name                   string
		conversationID, tenant string
		lowUser, highUser      string
		lowMember, highMember  string
		creator                string
	}{
		{"duplicate pair", directB, tenantA, adminA, personA, adminM, targetM2, adminA},
		{"reversed pair", directB, tenantA, personA, adminA, targetM2, adminM, adminA},
		{"self chat", directB, tenantA, adminA, adminA, adminM, adminM, adminA},
		{"foreign user", directB, tenantA, adminA, personB, adminM, otherM, adminA},
		{"wrong user membership", directB, tenantA, adminA, personA, targetM2, adminM, adminA},
		{"creator outside pair", directB, tenantA, adminA, personA, adminM, targetM2, personB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := conn.Exec(context.Background(), insert, tc.conversationID, tc.tenant,
				tc.lowUser, tc.highUser, tc.lowMember, tc.highMember, tc.creator); err == nil {
				t.Fatal("invalid direct conversation accepted")
			}
		})
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE tenant_id=$1", tenantA).Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid conversation changed table: %d %v", count, err)
	}
}

func TestDirectConversationMigrationRollsBackAndReapplies(t *testing.T) {
	conn := db(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../db/migrations/000005_direct_conversations.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var relation *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('conversations')::text").Scan(&relation); err != nil || relation != nil {
		t.Fatalf("conversation table after down: %v %v", relation, err)
	}
	up, err := os.ReadFile("../../db/migrations/000005_direct_conversations.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	seed(t, conn)
}
