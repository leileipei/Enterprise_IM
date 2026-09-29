package policystore_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	groupA       = "00000000-0000-4000-8000-000000000801"
	groupUserC   = "00000000-0000-4000-8000-000000000802"
	groupMemberC = "00000000-0000-4000-8000-000000000803"
)

func insertGroup(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,kind,group_name,group_creator_membership_id,
  group_creator_organization_id,group_creator_legal_entity_id,created_by_user_id)
 VALUES ($1,$2,'group','项目群',$3,$4,$5,$6)`, groupA, tenantA, adminM, orgA, legalA, adminA)
}

func insertInterval(conn *pgx.Conn, id, tenant, conversation, user, membership, organization, legal, role, status string, join int64, leave any) error {
	_, err := conn.Exec(context.Background(), `INSERT INTO conversation_membership_intervals
 (id,tenant_id,conversation_id,user_id,source_membership_id,source_organization_id,source_legal_entity_id,
  role,status,join_seq,leave_seq,joined_policy_version,left_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,0,
  CASE WHEN $11::bigint IS NULL THEN NULL ELSE now() END)`,
		id, tenant, conversation, user, membership, organization, legal, role, status, join, leave)
	return err
}

func TestGroupSchemaMembershipIntervalsAndIsolation(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO users (id,tenant_id,global_employee_no,display_name) VALUES ($1,$2,'A003','用户 C')", groupUserC, tenantA)
	run(t, conn, "INSERT INTO user_organizations (id,tenant_id,user_id,organization_id,effective_from) VALUES ($1,$2,$3,$4,'2020-01-01')", groupMemberC, tenantA, groupUserC, orgA)
	insertGroup(t, conn)
	reject(t, conn, "UPDATE conversations SET group_creator_membership_id=$1,group_creator_organization_id=$2 WHERE id=$3", targetM2, orgA2, groupA)
	reject(t, conn, "UPDATE conversations SET kind='direct' WHERE id=$1", groupA)
	if err := insertInterval(conn, "00000000-0000-4000-8000-000000000811", tenantA, groupA, adminA, adminM, orgA, legalA, "owner", "active", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := insertInterval(conn, "00000000-0000-4000-8000-000000000812", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "left", 1, int64(0)); err != nil {
		t.Fatalf("empty interval should be valid: %v", err)
	}
	if err := insertInterval(conn, "00000000-0000-4000-8000-000000000813", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "active", 1, nil); err != nil {
		t.Fatalf("rejoin after empty interval: %v", err)
	}
	run(t, conn, "UPDATE conversation_membership_intervals SET status='left',leave_seq=2,left_at=now() WHERE id='00000000-0000-4000-8000-000000000813'")
	reject(t, conn, "UPDATE conversation_membership_intervals SET leave_seq=3 WHERE id='00000000-0000-4000-8000-000000000813'")
	reject(t, conn, "UPDATE conversation_membership_intervals SET status='active',leave_seq=NULL,left_at=NULL WHERE id='00000000-0000-4000-8000-000000000813'")
	reject(t, conn, "DELETE FROM conversation_membership_intervals WHERE id='00000000-0000-4000-8000-000000000813'")
	if err := insertInterval(conn, "00000000-0000-4000-8000-000000000814", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "active", 4, nil); err != nil {
		t.Fatalf("rejoin after history gap: %v", err)
	}
	for seq, want := range map[int64]int{2: 1, 3: 0, 4: 1} {
		var count int
		if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3
   AND join_seq <= $4 AND (leave_seq IS NULL OR leave_seq >= $4)`, tenantA, groupA, personA, seq).Scan(&count); err != nil || count != want {
			t.Fatalf("visible interval at seq %d: got %d want %d: %v", seq, count, want, err)
		}
	}
	for _, tc := range []struct {
		name, tenant, conversation, user, membership, organization, legal, role, status string
		join                                                                            int64
		leave                                                                           any
	}{
		{"wrong tenant", tenantB, groupA, personB, otherM, orgB, legalB, "member", "active", 2, nil},
		{"wrong user membership", tenantA, groupA, adminA, targetM2, orgA, legalA, "member", "active", 2, nil},
		{"wrong organization", tenantA, groupA, personA, targetM2, orgA2, legalA, "member", "active", 2, nil},
		{"wrong legal snapshot", tenantA, groupA, personA, targetM2, orgA, legalB, "member", "active", 2, nil},
		{"duplicate active member", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "active", 2, nil},
		{"second active owner", tenantA, groupA, groupUserC, groupMemberC, orgA, legalA, "owner", "active", 2, nil},
		{"overlap after rejoin", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "left", 2, int64(4)},
		{"overlapping history", tenantA, groupA, adminA, adminM, orgA, legalA, "member", "left", 1, int64(2)},
		{"backward interval", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "left", 3, int64(1)},
		{"invalid role", tenantA, groupA, personA, targetM2, orgA, legalA, "superuser", "left", 3, int64(3)},
		{"invalid status", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "unknown", 3, int64(3)},
		{"active with end", tenantA, groupA, personA, targetM2, orgA, legalA, "member", "active", 3, int64(3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insertInterval(conn, "00000000-0000-4000-8000-000000000820", tc.tenant, tc.conversation, tc.user, tc.membership, tc.organization, tc.legal, tc.role, tc.status, tc.join, tc.leave); err == nil {
				t.Fatal("invalid group interval accepted")
			}
		})
	}
	// A direct conversation cannot be used as a group membership target.
	run(t, conn, `INSERT INTO conversations
 (id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id)
 VALUES ($1,$2,$3,$4,$5,$6,$3)`, directA, tenantA, adminA, personA, adminM, targetM2)
	if err := insertInterval(conn, "00000000-0000-4000-8000-000000000821", tenantA, directA, personA, targetM2, orgA, legalA, "member", "active", 2, nil); err == nil {
		t.Fatal("direct conversation accepted group interval")
	}
	reject(t, conn, `INSERT INTO conversations
	 (tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
	  group_creator_legal_entity_id,direct_user_low_id,created_by_user_id)
	 VALUES ($1,'group','mixed',$2,$3,$4,$5,$6)`, tenantA, adminM, orgA, legalA, adminA, adminA)
	reject(t, conn, `INSERT INTO conversations
 (tenant_id,kind,group_name,direct_user_low_id,direct_user_high_id,direct_low_membership_id,
  direct_high_membership_id,created_by_user_id) VALUES ($1,'direct','mixed',$2,$3,$4,$5,$2)`, tenantA, adminA, personA, adminM, targetM2)
	reject(t, conn, `INSERT INTO conversations
	 (tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
	  group_creator_legal_entity_id,created_by_user_id)
	 VALUES ($1,'group','   ',$2,$3,$4,$5)`, tenantA, adminM, orgA, legalA, adminA)
	reject(t, conn, `INSERT INTO conversations
	 (tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
	  group_creator_legal_entity_id,created_by_user_id)
	 VALUES ($1,'group','错误法人',$2,$3,$4,$5)`, tenantA, adminM, orgA, legalB, adminA)
	reject(t, conn, `INSERT INTO conversations
	 (tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
	  group_creator_legal_entity_id,created_by_user_id)
	 VALUES ($1,'group','错误任职',$2,$3,$4,$5)`, tenantA, targetM2, orgA, legalA, adminA)
	reject(t, conn, `INSERT INTO conversations
	 (tenant_id,kind,group_name,group_creator_membership_id,group_creator_organization_id,
	  group_creator_legal_entity_id,created_by_user_id)
	 VALUES ($1,'group','错误组织',$2,$3,$4,$5)`, tenantA, adminM, orgA2, legalA, adminA)
}

func TestGroupSchemaDownRefusesDataAndReapplies(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	insertGroup(t, conn)
	requestDown, err := os.ReadFile("../../db/migrations/000010_group_create_request.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(requestDown)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../db/migrations/000009_group_membership.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err == nil {
		t.Fatal("down migration removed group data")
	}
	if _, err := conn.Exec(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM conversations WHERE kind='group'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("group data after rejected down: %d %v", count, err)
	}
	run(t, conn, "DELETE FROM conversations WHERE id=$1", groupA)
	if _, err := conn.PgConn().Exec(context.Background(), string(down)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var relation *string
	if err := conn.QueryRow(context.Background(), "SELECT to_regclass('conversation_membership_intervals')::text").Scan(&relation); err != nil || relation != nil {
		t.Fatalf("group table after down: %v %v", relation, err)
	}
	up, err := os.ReadFile("../../db/migrations/000009_group_membership.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(up)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	requestUp, err := os.ReadFile("../../db/migrations/000010_group_create_request.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(context.Background(), string(requestUp)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	insertGroup(t, conn)
}
