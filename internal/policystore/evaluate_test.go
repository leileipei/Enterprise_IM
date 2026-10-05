package policystore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func request(target string) policystore.Request {
	return policystore.Request{Identity: publisher(), TargetMembershipID: target,
		Action: policy.ActionStartChat, ScopeAllowed: true, ResourceActive: true}
}

func TestEvaluateCurrentUsesDefaultBoundaryAndAudits(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	same, err := svc.EvaluateCurrent(context.Background(), request(targetM2))
	if err != nil || !same.Allowed || same.Reason != policy.ReasonAllowedSameOrganization || same.PolicyVersion != 0 {
		t.Fatalf("same organization: %+v %v", same, err)
	}
	cross, err := svc.EvaluateCurrent(context.Background(), request(targetM))
	if err != nil || cross.Allowed || cross.Reason != policy.ReasonCrossOrganizationDenied || cross.PolicyVersion != 0 {
		t.Fatalf("cross organization: %+v %v", cross, err)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE tenant_id=$1 AND policy_version IS NULL", tenantA).Scan(&count); err != nil || count != 2 {
		t.Fatalf("default decisions not audited: %d %v", count, err)
	}
}

func TestEvaluateCurrentPublishedAllowAndHardDeny(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "allow-1", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目联系",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放项目联系"); err != nil {
		t.Fatal(err)
	}
	decision, err := svc.EvaluateCurrent(context.Background(), request(targetM))
	if err != nil || !decision.Allowed || decision.Reason != policy.ReasonAllowedRule || decision.PolicyVersion != 1 ||
		len(decision.MatchedRuleIDs) != 1 || decision.MatchedRuleIDs[0] != "allow-1" {
		t.Fatalf("published allow: %+v %v", decision, err)
	}
	hard := policy.Rule{ID: "hard-1", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "强制隔离", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "强制隔离"); err != nil {
		t.Fatal(err)
	}
	decision, err = svc.EvaluateCurrent(context.Background(), request(targetM))
	if err != nil || decision.Allowed || decision.Reason != policy.ReasonHardDeny || decision.PolicyVersion != 2 ||
		len(decision.MatchedRuleIDs) != 1 || decision.MatchedRuleIDs[0] != "hard-1" {
		t.Fatalf("hard deny: %+v %v", decision, err)
	}
	var version int64
	var allowed bool
	var matched []string
	if err := conn.QueryRow(context.Background(), "SELECT policy_version,allowed,matched_rule_ids FROM policy_decision_events WHERE tenant_id=$1 ORDER BY id DESC LIMIT 1", tenantA).Scan(&version, &allowed, &matched); err != nil || version != 2 || allowed || len(matched) != 1 || matched[0] != "hard-1" {
		t.Fatalf("decision audit: version=%d allowed=%t matched=%v err=%v", version, allowed, matched, err)
	}
}

func TestEvaluateCurrentExpiredExceptionAndCrossTenantTarget(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	current := at
	svc := policystore.Service{DB: conn, Now: func() time.Time { return current }}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolationRule(), exceptionRule()}, "项目临时例外"); err != nil {
		t.Fatal(err)
	}
	decision, err := svc.EvaluateCurrent(context.Background(), request(targetM))
	if err != nil || !decision.Allowed || decision.Reason != policy.ReasonAllowedException {
		t.Fatalf("live exception: %+v %v", decision, err)
	}
	current = at.Add(2 * time.Hour)
	decision, err = svc.EvaluateCurrent(context.Background(), request(targetM))
	if err != nil || decision.Allowed || decision.Reason != policy.ReasonIsolated {
		t.Fatalf("expired exception: %+v %v", decision, err)
	}
	decision, err = svc.EvaluateCurrent(context.Background(), request(otherM))
	if err != nil || decision.Allowed || decision.Reason != policy.ReasonInvalidContext {
		t.Fatalf("cross-tenant target exposed: %+v %v", decision, err)
	}
}

func TestEvaluateCurrentAuditFailureNeverReturnsAllow(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "ALTER TABLE policy_decision_events ADD CONSTRAINT forbid_allowed_audit CHECK (allowed = false)")
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	decision, err := svc.EvaluateCurrent(context.Background(), request(targetM2))
	if !errors.Is(err, policystore.ErrAuditUnavailable) || decision.Allowed {
		t.Fatalf("allowed without durable audit: %+v %v", decision, err)
	}
}
