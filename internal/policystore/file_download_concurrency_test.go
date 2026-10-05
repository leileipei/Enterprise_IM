package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func TestFileDownloadConcurrentPolicyChange(t *testing.T) {
	for _, what := range []string{"ttl", "uploader", "requester", "conversation"} {
		t.Run(what, func(t *testing.T) {
			c, s, cid, m := fileHistoryFixture(t, "direct")
			peer := filePeer(t, c)
			ctx := context.Background()
			ticket := beginDownload(t, c, m, groupMemberIdentity())
			if e := s.AuthorizeFileDownload(ctx, groupMemberIdentity(), ticket); e != nil {
				t.Fatal(e)
			}
			tx, e := c.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			q := `UPDATE tenant_file_retention_policy SET file_retention_days=1,version=version+1,approval_reference='CONCURRENT',actor_user_id=$2,acting_membership_id=$3,updated_at=clock_timestamp() WHERE tenant_id=$1`
			args := []any{tenantA, adminA, adminM}
			switch what {
			case "uploader":
				q = "UPDATE user_organizations SET status='suspended' WHERE id=$1"
				args = []any{adminM}
			case "requester":
				q = "UPDATE user_organizations SET status='suspended' WHERE id=$1"
				args = []any{targetM2}
			case "conversation":
				q = "UPDATE conversations SET status='ended' WHERE id=$1"
				args = []any{cid}
			}
			if _, e = tx.Exec(ctx, q, args...); e != nil {
				t.Fatal(e)
			}
			other := policystore.Service{DB: peer}
			var waiting chan error
			if what == "ttl" || what == "requester" {
				waiting = make(chan error, 1)
				go func() { waiting <- other.CheckFileDownload(ctx, groupMemberIdentity(), ticket) }()
				waitFileLock(t, c, peer, "", func() { tx.Rollback(ctx) })
			} else if e = other.CheckFileDownload(ctx, groupMemberIdentity(), ticket); e == nil {
				t.Fatal("check crossed uncommitted revocation lock")
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			if waiting != nil {
				if e = <-waiting; e == nil {
					t.Fatal("stale policy wait")
				}
			}
			if e = other.CheckFileDownload(ctx, groupMemberIdentity(), ticket); e == nil {
				t.Fatal("check used stale preflight")
			}
		})
	}
}
func TestFileDownloadAuditWaitExpiry(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	peer := filePeer(t, c)
	ctx := context.Background()
	deadline := downloadDeadline(t, c, 1500*time.Millisecond)
	ticket, e := s.BeginFileDownload(ctx, publisher(), m.ID, uploadOwner, deadline)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE"); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() { ch <- (policystore.Service{DB: peer}).AuthorizeFileDownload(ctx, publisher(), ticket) }()
	waitFileLock(t, c, peer, "RowExclusiveLock", func() { tx.Rollback(ctx) })
	if _, e = tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, deadline); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; e == nil {
		t.Fatal("audit wait admitted expired grant")
	}
	var phase string
	var grants int
	if e = c.QueryRow(ctx, `SELECT phase,(SELECT count(*) FROM audit_events WHERE action='file_download_authorize') FROM file_download_sessions WHERE id=$1`, ticket.SessionID).Scan(&phase, &grants); e != nil || phase != "preparing" || grants != 0 {
		t.Fatal(phase, grants, e)
	}
}
func TestFileDownloadMultiNodeGate(t *testing.T) { TestFileDownloadSessionLimits(t) }
func TestFileRetentionConcurrentChange(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ticket := beginDownload(t, c, m, publisher())
	if e := s.AuthorizeFileDownload(context.Background(), publisher(), ticket); e != nil {
		t.Fatal(e)
	}
	peer := filePeer(t, c)
	tx, e := c.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(context.Background(), `UPDATE tenant_file_retention_policy SET file_retention_days=1,version=version+1,approval_reference='CAB',actor_user_id=$2,acting_membership_id=$3,updated_at=clock_timestamp() WHERE tenant_id=$1`, tenantA, adminA, adminM); e != nil {
		t.Fatal(e)
	}
	waiting := make(chan error, 1)
	go func() {
		waiting <- (policystore.Service{DB: peer}).CheckFileDownload(context.Background(), publisher(), ticket)
	}()
	waitFileLock(t, c, peer, "", func() { tx.Rollback(context.Background()) })
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = <-waiting; !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("policy change after wait", e)
	}
	if e = (policystore.Service{DB: peer}).CheckFileDownload(context.Background(), publisher(), ticket); !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("fresh TTL", e)
	}
}

func TestFileDownloadConcurrentHardDeny(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	peer := filePeer(t, c)
	ctx := context.Background()
	ticket := beginDownload(t, c, m, groupMemberIdentity())
	if e := s.AuthorizeFileDownload(ctx, groupMemberIdentity(), ticket); e != nil {
		t.Fatal(e)
	}
	grantPublisher(t, c)
	r := policy.Rule{ID: "concurrent-download-deny", TenantID: tenantA, Action: policy.ActionFileDownload, Effect: policy.EffectHardDeny, SourceMembershipID: targetM2, TargetMembershipID: adminM, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Hour), Reason: "download revoked"}
	if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{r}, "CAB-DOWNLOAD"); e != nil {
		t.Fatal(e)
	}
	if e := (policystore.Service{DB: peer}).CheckFileDownload(ctx, groupMemberIdentity(), ticket); !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("published denial ignored after external read", e)
	}
}
