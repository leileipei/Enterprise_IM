package policystore_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

const visibleRoot = "00000000-0000-4000-8000-000000000291"

type organizationSnapshotGateDB struct {
	conn    *pgx.Conn
	reached chan struct{}
	resume  chan struct{}
}

func (db organizationSnapshotGateDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return organizationSnapshotGateTx{Tx: tx, reached: db.reached, resume: db.resume}, nil
}

type organizationSnapshotGateTx struct {
	pgx.Tx
	reached chan struct{}
	resume  chan struct{}
}

func (tx organizationSnapshotGateTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "FROM organizations WHERE tenant_id=$1") {
		return rows, err
	}
	return organizationSnapshotGateRows{Rows: rows, reached: tx.reached, resume: tx.resume}, nil
}

type organizationSnapshotGateRows struct {
	pgx.Rows
	reached chan struct{}
	resume  chan struct{}
}

func (rows organizationSnapshotGateRows) Close() {
	rows.Rows.Close()
	close(rows.reached)
	<-rows.resume
}

func TestVisibleOrganizationsPinsAncestorStatusDuringProjection(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,org_type,code,name) VALUES ($1,$2,'virtual_group','root','集团根')", visibleRoot, tenantA)
	run(t, conn, "UPDATE organizations SET parent_id=$1 WHERE id=$2", visibleRoot, orgA)
	ctx := context.Background()
	var searchPath string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	updater, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { updater.Close(ctx) })
	if _, err := updater.Exec(ctx, "SET search_path TO "+searchPath); err != nil {
		t.Fatal(err)
	}
	reached, resume := make(chan struct{}), make(chan struct{})
	svc := policystore.Service{DB: organizationSnapshotGateDB{conn: conn, reached: reached, resume: resume}, Now: func() time.Time { return at }}
	type result struct {
		nodes []policystore.DirectoryOrganization
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		nodes, err := svc.ListVisibleOrganizations(ctx, publisher())
		finished <- result{nodes: nodes, err: err}
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("organization snapshot was not read")
	}
	_, updateErr := updater.Exec(ctx, "SET statement_timeout='150ms'")
	if updateErr == nil {
		_, updateErr = updater.Exec(ctx, "UPDATE organizations SET status='disabled' WHERE id=$1", visibleRoot)
	}
	close(resume)
	got := <-finished
	if updateErr == nil {
		t.Fatal("ancestor was disabled while directory projection used its old status")
	}
	var pgErr *pgconn.PgError
	if !errors.As(updateErr, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("expected blocked ancestor update, got %v", updateErr)
	}
	if got.err != nil || len(got.nodes) != 2 || got.nodes[0].ID != visibleRoot || got.nodes[1].ID != orgA {
		t.Fatalf("organization projection changed after locked snapshot: %+v %v", got.nodes, got.err)
	}
}

func TestVisibleOrganizationsIncludeOnlyVisibleBranchAndActiveAncestors(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	run(t, conn, "INSERT INTO organizations (id,tenant_id,org_type,code,name) VALUES ($1,$2,'virtual_group','root','集团根')", visibleRoot, tenantA)
	run(t, conn, "UPDATE organizations SET parent_id=$1 WHERE id IN ($2,$3)", visibleRoot, orgA, orgA2)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	nodes, err := svc.ListVisibleOrganizations(context.Background(), publisher())
	if err != nil || len(nodes) != 2 || nodes[0].ID != visibleRoot || nodes[0].ParentID != "" || nodes[0].HasVisibleMembers ||
		nodes[1].ID != orgA || nodes[1].ParentID != visibleRoot || !nodes[1].HasVisibleMembers {
		t.Fatalf("visible branch: %+v %v", nodes, err)
	}
	var decisions, requests int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events WHERE action='directory_view'").Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("directory decisions: %d %v", decisions, err)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='directory_organization_list' AND outcome='allow' AND resource_id IS NULL").Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("organization request audit: %d %v", requests, err)
	}
	run(t, conn, "UPDATE organizations SET status='disabled' WHERE id=$1", visibleRoot)
	nodes, err = svc.ListVisibleOrganizations(context.Background(), publisher())
	if err != nil || len(nodes) != 1 || nodes[0].ID != orgA || nodes[0].ParentID != "" {
		t.Fatalf("disabled ancestor exposed: %+v %v", nodes, err)
	}
}

func TestVisibleOrganizationsFollowPublishedAllowHardDenyAndBidirectionalRule(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	grantPublisher(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	allow := policy.Rule{ID: "org-allow", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		RequestedBy: adminA, ApprovedBy: adminA, Reason: "跨组织目录",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 0, []policy.Rule{allow}, "开放组织目录"); err != nil {
		t.Fatal(err)
	}
	nodes, err := svc.ListVisibleOrganizations(context.Background(), publisher())
	if err != nil || len(nodes) != 2 || nodes[0].ID != orgA || nodes[1].ID != orgA2 || !nodes[1].HasVisibleMembers {
		t.Fatalf("published allow not applied: %+v %v", nodes, err)
	}
	hard := policy.Rule{ID: "org-deny", TenantID: tenantA, Effect: policy.EffectHardDeny,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA, TargetOrganizationID: orgA2,
		Reason: "隔离组织", EffectiveFrom: at.Add(-time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 1, []policy.Rule{allow, hard}, "隔离组织目录"); err != nil {
		t.Fatal(err)
	}
	nodes, err = svc.ListVisibleOrganizations(context.Background(), publisher())
	if err != nil || len(nodes) != 1 || nodes[0].ID != orgA {
		t.Fatalf("hard deny bypassed: %+v %v", nodes, err)
	}
	reverse := policy.Rule{ID: "org-reverse", TenantID: tenantA, Effect: policy.EffectAllow,
		Action: policy.ActionDirectoryView, SourceOrganizationID: orgA2, TargetOrganizationID: orgA,
		Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, Reason: "双向目录",
		EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour)}
	if _, err := svc.Publish(context.Background(), publisher(), 2, []policy.Rule{reverse}, "双向开放组织"); err != nil {
		t.Fatal(err)
	}
	nodes, err = svc.ListVisibleOrganizations(context.Background(), publisher())
	if err != nil || len(nodes) != 2 || nodes[1].ID != orgA2 {
		t.Fatalf("bidirectional allow missed: %+v %v", nodes, err)
	}
}

func TestVisibleOrganizationsRejectInvalidActorAndAuditFailures(t *testing.T) {
	conn := db(t)
	seed(t, conn)
	svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
	run(t, conn, "UPDATE user_organizations SET status='ended' WHERE id=$1", adminM)
	if nodes, err := svc.ListVisibleOrganizations(context.Background(), publisher()); !errors.Is(err, policystore.ErrForbidden) || len(nodes) != 0 {
		t.Fatalf("ended actor accepted: %+v %v", nodes, err)
	}
	for _, tc := range []struct{ name, constraint string }{
		{"request", "ALTER TABLE audit_events ADD CONSTRAINT reject_org_list CHECK (action <> 'directory_organization_list')"},
		{"decision", "ALTER TABLE policy_decision_events ADD CONSTRAINT reject_org_decision CHECK (action <> 'directory_view')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := db(t)
			seed(t, conn)
			run(t, conn, tc.constraint)
			svc := policystore.Service{DB: conn, Now: func() time.Time { return at }}
			nodes, err := svc.ListVisibleOrganizations(context.Background(), publisher())
			if !errors.Is(err, policystore.ErrAuditUnavailable) || len(nodes) != 0 {
				t.Fatalf("nodes returned without audit: %+v %v", nodes, err)
			}
			var count int
			if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM policy_decision_events").Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial decision audit committed: %d %v", count, err)
			}
		})
	}
}
