package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestRecheckGroupPolicyRestoresOnlyAfterAllPairsAllow(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	seedThirdGroupMember(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2, groupMemberC))
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "pair-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA,
		SourceMembershipID: targetM2, TargetMembershipID: groupMemberC,
		EffectiveFrom: at.Add(-time.Hour), Reason: "conflict"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "deny pair"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("conflicting pair restored: %v", err)
	}
	var status string
	var version int64
	if err := conn.QueryRow(context.Background(), "SELECT status,last_policy_version FROM conversations WHERE id=$1", group.ID).Scan(&status, &version); err != nil || status != "policy_blocked" || version != 1 {
		t.Fatalf("blocked state/version: %s %d %v", status, version, err)
	}
	if _, err := svc.RemoveGroupMember(context.Background(), publisher(), group.ID, groupIntervalFor(t, conn, group.ID, groupUserC)); err != nil {
		t.Fatal(err)
	}
	result, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID)
	if err != nil || result.Status != "active" || result.PolicyVersion != 1 {
		t.Fatalf("restore after removal: %+v %v", result, err)
	}
	if _, err := svc.SendGroupTextMessage(context.Background(), publisher(), group.ID, clientUUIDv7(at, 941), "已恢复"); err != nil {
		t.Fatalf("send after restore: %v", err)
	}
	var allowAudits, denyAudits int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE outcome='allow'),count(*) FILTER (WHERE outcome='deny')
 FROM audit_events WHERE action='group_policy_recheck' AND resource_id=$1`, group.ID).Scan(&allowAudits, &denyAudits); err != nil || allowAudits != 1 || denyAudits != 1 {
		t.Fatalf("recheck audits: %d %d %v", allowAudits, denyAudits, err)
	}
}

func TestRecheckGroupPolicyEnforcesRoleIdentityAndTenant(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	if _, err := svc.RecheckGroupPolicy(context.Background(), groupMemberIdentity(), group.ID); !errors.Is(err, policystore.ErrGroupRecheckPermissionDenied) {
		t.Fatalf("ordinary member rechecked: %v", err)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	if _, err := svc.RecheckGroupPolicy(context.Background(), foreign, group.ID); !errors.Is(err, policystore.ErrGroupNotAvailable) {
		t.Fatalf("foreign tenant saw group: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(-time.Second), adminM)
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired actor rechecked: %v", err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "policy_blocked" {
		t.Fatalf("unauthorized restore: %s %v", status, err)
	}
}

func TestRecheckGroupPolicyRestoresAfterNewPublishedPolicy(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA,
		TargetOrganizationID: orgA, TargetMembershipID: targetM2,
		EffectiveFrom: at.Add(-time.Hour), Reason: "blocked"}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "block"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("initial policy not enforced: %v", err)
	}
	if _, err := svc.Publish(context.Background(), publisher(), 1, nil, "remove deny"); err != nil {
		t.Fatal(err)
	}
	result, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID)
	if err != nil || result.Status != "active" || result.PolicyVersion != 2 {
		t.Fatalf("new policy did not restore group: %+v %v", result, err)
	}
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); err != nil {
		t.Fatalf("repeat recheck failed: %v", err)
	}
}

func TestRecheckGroupPolicyDoesNotRestoreExpiredMemberOrCommitWithoutAudit(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := svc.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(-time.Second), targetM2)
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("expired source membership restored: %v", err)
	}
	run(t, conn, "UPDATE user_organizations SET effective_to=NULL WHERE id=$1", targetM2)
	run(t, conn, `CREATE FUNCTION fail_recheck_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.action='group_policy_recheck' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER fail_recheck_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION fail_recheck_audit()`)
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); !errors.Is(err, policystore.ErrAuditUnavailable) {
		t.Fatalf("audit failure ignored: %v", err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "policy_blocked" {
		t.Fatalf("restored without audit: %s %v", status, err)
	}
}

func TestRecheckGroupPolicyRechecksTimedDenyAfterAllowAudit(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	rule := policy.Rule{ID: "later-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionSendMessage, SourceOrganizationID: orgA,
		TargetOrganizationID: orgA, TargetMembershipID: targetM2,
		EffectiveFrom: at.Add(time.Second), Reason: "later"}
	if _, err := creator.Publish(context.Background(), publisher(), 0, []policy.Rule{rule}, "later"); err != nil {
		t.Fatal(err)
	}
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		checks++
		if checks >= 4 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	if _, err := svc.RecheckGroupPolicy(context.Background(), publisher(), group.ID); !errors.Is(err, policystore.ErrGroupPolicyBlocked) {
		t.Fatalf("timed deny bypassed: %v", err)
	}
	var status string
	var allows, denies int
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "policy_blocked" {
		t.Fatalf("timed deny status: %s %v", status, err)
	}
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE outcome='allow'),count(*) FILTER (WHERE outcome='deny')
 FROM audit_events WHERE action='group_policy_recheck' AND resource_id=$1`, group.ID).Scan(&allows, &denies); err != nil || allows != 0 || denies != 1 {
		t.Fatalf("provisional allow audit persisted: %d %d %v", allows, denies, err)
	}
}

func TestRecheckGroupPolicyAllowsAdministratorButNotExpiredActorAtCommit(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	creator := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	group, err := creator.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
	if err != nil {
		t.Fatal(err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	run(t, conn, "UPDATE conversation_membership_intervals SET role='admin' WHERE conversation_id=$1 AND user_id=$2", group.ID, personA)
	if result, err := creator.RecheckGroupPolicy(context.Background(), groupMemberIdentity(), group.ID); err != nil || result.Status != "active" {
		t.Fatalf("administrator cannot restore: %+v %v", result, err)
	}
	run(t, conn, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", group.ID)
	run(t, conn, "UPDATE user_organizations SET effective_to=$1 WHERE id=$2", at.Add(time.Second), targetM2)
	checks := 0
	svc := policystore.Service{DB: conn, Now: func() time.Time {
		checks++
		if checks >= 4 {
			return at.Add(2 * time.Second)
		}
		return at
	}}
	if _, err := svc.RecheckGroupPolicy(context.Background(), groupMemberIdentity(), group.ID); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("expired administrator restored: %v", err)
	}
	var status string
	if err := conn.QueryRow(context.Background(), "SELECT status FROM conversations WHERE id=$1", group.ID).Scan(&status); err != nil || status != "policy_blocked" {
		t.Fatalf("expired administrator changed group: %s %v", status, err)
	}
}
