package policystore_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func publisher() access.TrustedIdentity {
	return access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
}

func grantPublisher(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	run(t, conn, "INSERT INTO admin_grants (id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES ('00000000-0000-4000-8000-000000000251',$1,$2,$3,'group_admin','2020-01-01')", tenantA, adminM, orgA)
}

func isolationRule() policy.Rule {
	return policy.Rule{ID: "isolate-1", TenantID: tenantA, Effect: policy.EffectIsolate,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "组织隔离", EffectiveFrom: at.Add(-time.Hour)}
}

func exceptionRule() policy.Rule {
	return policy.Rule{ID: "exception-1", TenantID: tenantA, Effect: policy.EffectExceptionAllow,
		Action: policy.ActionStartChat, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		SourceMembershipID: adminM, TargetMembershipID: targetM,
		OverrideRuleID: "isolate-1", RequestedBy: adminA, ApprovedBy: adminA, Reason: "项目协作",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
}

func TestPublishCompleteSnapshotAndAudit(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	version, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolationRule(), exceptionRule()}, "首版发布")
	if err != nil || version != 1 {
		t.Fatalf("publish version=%d err=%v", version, err)
	}
	var current, ruleCount, auditCount int
	if err := conn.QueryRow(context.Background(), "SELECT current_version FROM policy_current WHERE tenant_id=$1", tenantA).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_rules WHERE tenant_id=$1 AND version=1", tenantA).Scan(&ruleCount); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND action='policy_publish' AND outcome='allow'", tenantA).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if current != 1 || ruleCount != 2 || auditCount != 1 {
		t.Fatalf("snapshot incomplete: version=%d rules=%d audit=%d", current, ruleCount, auditCount)
	}
}

func TestPublishRejectsMissingAdminGrantAndInvalidException(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolationRule()}, "unauthorized"); !errors.Is(err, policystore.ErrForbidden) {
		t.Fatalf("non-admin published: %v", err)
	}
	grantPublisher(t, conn)
	invalid := exceptionRule()
	invalid.ApprovedBy = ""
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolationRule(), invalid}, "invalid"); !errors.Is(err, policystore.ErrInvalidRules) {
		t.Fatalf("pending exception published: %v", err)
	}
	invalid = exceptionRule()
	invalid.OverrideRuleID = "missing-isolation"
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{isolationRule(), invalid}, "invalid"); !errors.Is(err, policystore.ErrInvalidRules) {
		t.Fatalf("unrelated override published: %v", err)
	}
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_current WHERE tenant_id=$1", tenantA).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid policy became current: count=%d err=%v", count, err)
	}
}

func TestPublishExpectedVersionAndTenantBoundaries(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	if _, err := svc.Publish(context.Background(), publisher(), 0, nil, "空规则基线"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(context.Background(), publisher(), 0, nil, "过期版本"); !errors.Is(err, policystore.ErrVersionConflict) {
		t.Fatalf("stale publish accepted: %v", err)
	}
	foreign := isolationRule()
	foreign.SourceOrganizationID = orgB
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{foreign}, "跨租户规则"); err == nil {
		t.Fatal("cross-tenant rule published")
	}
	var current int
	if err := conn.QueryRow(context.Background(), "SELECT current_version FROM policy_current WHERE tenant_id=$1", tenantA).Scan(&current); err != nil || current != 1 {
		t.Fatalf("current version moved on failed publish: %d %v", current, err)
	}
}

func secondDB(t *testing.T, first *pgx.Conn) *pgx.Conn {
	t.Helper()
	var schema string
	if err := first.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(context.Background(), "SET search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestConcurrentPublishAcceptsOnlyOneExpectedVersion(t *testing.T) {
	first := db(t)
	seed(t, first)
	grantPublisher(t, first)
	second := secondDB(t, first)
	services := []policystore.Service{
		{DB: first, Now: func() time.Time { return at }},
		{DB: second, Now: func() time.Time { return at }},
	}
	results := make(chan error, 2)
	for _, svc := range services {
		go func(svc policystore.Service) {
			_, err := svc.Publish(context.Background(), publisher(), 0, nil, "并发发布")
			results <- err
		}(svc)
	}
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, policystore.ErrVersionConflict)) || (b == nil && errors.Is(a, policystore.ErrVersionConflict))) {
		t.Fatalf("concurrent results: %v, %v", a, b)
	}
}
