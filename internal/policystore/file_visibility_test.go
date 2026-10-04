package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func TestFileVisibilityFinalClock(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			c, _, _, m := fileHistoryFixture(t, kind)
			ctx := context.Background()
			var accepted time.Time
			var days int64
			if err := c.QueryRow(ctx, `SELECT m.accepted_at,p.file_retention_days FROM messages m JOIN message_attachments a ON a.message_id=m.id AND a.tenant_id=m.tenant_id JOIN tenant_file_retention_policy p ON p.tenant_id=m.tenant_id WHERE a.file_id=$1`, m.ID).Scan(&accepted, &days); err != nil {
				t.Fatal(err)
			}
			tx, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			evaluate, err := policystore.FileVisibilityForTest(ctx, tx, groupMemberIdentity(), m.ID)
			if err != nil {
				t.Fatal(err)
			}
			boundary, err := files.FileExpiresAt(accepted, days)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err = evaluate(boundary.Add(-time.Nanosecond)); err != nil {
				t.Fatal("premature expiry", err)
			}
			if _, _, _, err = evaluate(boundary); !errors.Is(err, filedownload.ErrNotFound) {
				t.Fatal("boundary reopened", err)
			}
		})
	}
	c, s, _, m := fileHistoryFixture(t, "group")
	grantPublisher(t, c)
	var begins time.Time
	ctx := context.Background()
	if err := c.QueryRow(ctx, "SELECT clock_timestamp()+interval '1 hour'").Scan(&begins); err != nil {
		t.Fatal(err)
	}
	rule := policy.Rule{ID: "visibility-future", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, EffectiveFrom: begins, Reason: "future"}
	if _, err := s.Publish(ctx, publisher(), 0, []policy.Rule{rule}, "test"); err != nil {
		t.Fatal(err)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	evaluate, err := policystore.FileVisibilityForTest(ctx, tx, groupMemberIdentity(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = evaluate(begins.Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = evaluate(begins); !errors.Is(err, filedownload.ErrNotFound) {
		t.Fatal("future group deny omitted", err)
	}
}
func TestFileVisibilitySourceMismatch(t *testing.T) {
	c, _, cid, m := fileHistoryFixture(t, "direct")
	other := freshFile().ID
	run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", other, tenantA, adminA, orgA2)
	run(t, c, "UPDATE conversations SET direct_low_membership_id=$2 WHERE id=$1", cid, other)
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = policystore.FileVisibilityForTest(ctx, tx, groupMemberIdentity(), m.ID); !errors.Is(err, filedownload.ErrNotFound) {
		t.Fatal("source mismatch admitted", err)
	}
}
func TestFileVisibilityNoSideEffects(t *testing.T) {
	c, _, _, m := fileHistoryFixture(t, "group")
	ctx := context.Background()
	var beforeSessions, beforeOutbox int64
	if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM file_download_sessions),(SELECT count(*) FROM outbox_events)`).Scan(&beforeSessions, &beforeOutbox); err != nil {
		t.Fatal(err)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	evaluate, err := policystore.FileVisibilityForTest(ctx, tx, groupMemberIdentity(), m.ID)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// The captured evaluator has no live transaction. Any attempted I/O would fail.
	got, mid, seq, err := evaluate(now)
	if err != nil || got.ID != m.ID || mid == "" || seq != 2 {
		t.Fatal(got.ID, mid, seq, err)
	}
	var sessions, outbox int64
	if err = c.QueryRow(ctx, `SELECT (SELECT count(*) FROM file_download_sessions),(SELECT count(*) FROM outbox_events)`).Scan(&sessions, &outbox); err != nil || sessions != beforeSessions || outbox != beforeOutbox {
		t.Fatal("proof caused side effects", sessions, outbox, err)
	}
}
