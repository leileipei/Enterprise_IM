package access_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

func filePolicyDB(t *testing.T) *pgx.Conn {
	t.Helper()
	c := testDB(t)
	for _, n := range []string{"000018_file_foundation", "000019_file_upload_scan"} {
		b, e := os.ReadFile("../../db/migrations/" + n + ".up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	seedAccess(t, c)
	run(t, c, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES($1,$2,$3,$4,'group_admin','2020-01-01')`, retentionGrant, tenantA, adminM, orgA)
	return c
}
func filePolicyChange() access.FileUploadPolicyChange {
	p := files.DefaultUploadPolicy()
	p.Enabled = true
	return access.FileUploadPolicyChange{Policy: p, ApprovalReference: "CAB-FILE-1"}
}
func TestFileUploadPolicyAdminCAS(t *testing.T) {
	c := filePolicyDB(t)
	svc := access.Service{DB: c}
	ctx := context.Background()
	p, e := svc.GetFileUploadPolicy(ctx, identity())
	if e != nil || p.Policy.Enabled || p.Policy.Version != 0 {
		t.Fatal(p, e)
	}
	ordinary := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	if _, e = svc.GetFileUploadPolicy(ctx, ordinary); !errors.Is(e, access.ErrNotFound) {
		t.Fatal(e)
	}
	if p, e := svc.GetEffectiveFileUploadPolicy(ctx, ordinary); e != nil || p.Enabled {
		t.Fatal(p, e)
	}
	c2 := secondConnection(t, c)
	out := make(chan error, 2)
	for _, peer := range []*pgx.Conn{c, c2} {
		go func(peer *pgx.Conn) {
			_, e := (access.Service{DB: peer}).SetFileUploadPolicy(ctx, identity(), filePolicyChange())
			out <- e
		}(peer)
	}
	var success, conflict int
	for i := 0; i < 2; i++ {
		e := <-out
		if e == nil {
			success++
		} else if errors.Is(e, access.ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	var history, audits int
	if e = c.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenant_file_upload_policy_history),(SELECT count(*) FROM audit_events WHERE action='file_upload_policy_update' AND outcome='allow')`).Scan(&history, &audits); e != nil || success != 1 || conflict != 1 || history != 1 || audits != 1 {
		t.Fatal(success, conflict, history, audits, e)
	}
	p, e = svc.GetFileUploadPolicy(ctx, identity())
	if e != nil || !p.Policy.Enabled || p.Policy.Version != 1 || p.ActorUserID != adminA || p.ActorMembershipID != adminM || p.ApprovalReference != "CAB-FILE-1" || p.UpdatedAt.IsZero() {
		t.Fatal(p, e)
	}
	if _, e = svc.SetFileUploadPolicy(ctx, ordinary, filePolicyChange()); !errors.Is(e, access.ErrNotFound) {
		t.Fatal("ordinary changed policy", e)
	}
}
func TestFileUploadPolicyInvalidChanges(t *testing.T) {
	c := filePolicyDB(t)
	svc := access.Service{DB: c}
	for _, ref := range []string{"", " approval", "approval ", "approval\n", strings.Repeat("中", 43)} {
		v := filePolicyChange()
		v.ApprovalReference = ref
		if _, e := svc.SetFileUploadPolicy(context.Background(), identity(), v); !errors.Is(e, files.ErrInvalidUploadPolicy) {
			t.Fatal(ref, e)
		}
	}
	v := filePolicyChange()
	v.Policy.AllowedMediaTypes = []string{"application/zip"}
	if _, e := svc.SetFileUploadPolicy(context.Background(), identity(), v); !errors.Is(e, files.ErrInvalidUploadPolicy) {
		t.Fatal(e)
	}
	v = filePolicyChange()
	v.ExpectedVersion = -1
	if _, e := svc.SetFileUploadPolicy(context.Background(), identity(), v); !errors.Is(e, files.ErrInvalidUploadPolicy) {
		t.Fatal(e)
	}
}
func TestFileUploadPolicyBudget(t *testing.T) {
	c := filePolicyDB(t)
	ctx := context.Background()
	chat := "00000000-0000-4000-8000-00000000f031"
	run(t, c, `INSERT INTO conversations(id,tenant_id,direct_user_low_id,direct_user_high_id,direct_low_membership_id,direct_high_membership_id,created_by_user_id) VALUES($1,$2,$3,$4,$5,$6,$3)`, chat, tenantA, adminA, personA, adminM, personM)
	for i, size := range []int64{26214400, 1} {
		fid := fmt.Sprintf("00000000-0000-4000-8000-%012d", 900+i)
		p := files.CreateParams{TenantID: tenantA, ConversationID: chat, UploaderUserID: adminA, UploaderMembershipID: adminM, UploadRequestID: fid, OriginalFilename: "expired.txt", DeclaredMediaType: "text/plain", DeclaredSizeBytes: size}
		d, e := files.CreationDigest(p)
		if e != nil {
			t.Fatal(e)
		}
		run(t, c, `INSERT INTO file_objects(id,tenant_id,conversation_id,uploader_user_id,uploader_membership_id,upload_request_id,request_digest,original_filename,declared_media_type,declared_size_bytes,state,state_version,created_at,updated_at,upload_expires_at) VALUES($1,$2,$3,$4,$5,$1,$6,'expired.txt','text/plain',$7,'allocated',0,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')`, fid, tenantA, chat, adminA, adminM, d[:], size)
	}
	change := filePolicyChange()
	change.Policy.TenantStorageBudgetBytes = 26214400
	if _, e := (access.Service{DB: c}).SetFileUploadPolicy(ctx, identity(), change); !errors.Is(e, files.ErrStorageBudgetExceeded) {
		t.Fatal("expired reservations not charged", e)
	}
	var version int64
	if e := c.QueryRow(ctx, `SELECT version FROM tenant_file_upload_policy WHERE tenant_id=$1`, tenantA).Scan(&version); e != nil || version != 0 {
		t.Fatal(version, e)
	}
}
func TestFileUploadPolicyAtomicAudit(t *testing.T) {
	c := filePolicyDB(t)
	run(t, c, `CREATE FUNCTION fail_file_policy_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_upload_policy_update' THEN RAISE EXCEPTION 'injected audit failure';END IF;RETURN NEW;END $$;CREATE TRIGGER fail_file_policy_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_file_policy_audit()`)
	if _, e := (access.Service{DB: c}).SetFileUploadPolicy(context.Background(), identity(), filePolicyChange()); !errors.Is(e, access.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	var v, h int
	if e := c.QueryRow(context.Background(), `SELECT (SELECT version FROM tenant_file_upload_policy WHERE tenant_id=$1),(SELECT count(*) FROM tenant_file_upload_policy_history)`, tenantA).Scan(&v, &h); e != nil || v != 0 || h != 0 {
		t.Fatal(v, h, e)
	}
}
func TestFileUploadPolicyWaitExpiry(t *testing.T) {
	for _, kind := range []string{"membership", "grant"} {
		t.Run(kind, func(t *testing.T) {
			c := filePolicyDB(t)
			peer := secondConnection(t, c)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var expires time.Time
			if e := c.QueryRow(ctx, `SELECT clock_timestamp()+interval '1 second'`).Scan(&expires); e != nil {
				t.Fatal(e)
			}
			if kind == "membership" {
				run(t, c, `UPDATE user_organizations SET effective_to=$2 WHERE id=$1`, adminM, expires)
			} else {
				run(t, c, `UPDATE admin_grants SET effective_to=$2 WHERE id=$1`, retentionGrant, expires)
			}
			run(t, c, "BEGIN")
			run(t, c, `SELECT tenant_id FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE`, tenantA)
			out := make(chan error, 1)
			go func() {
				_, e := (access.Service{DB: peer}).SetFileUploadPolicy(ctx, identity(), filePolicyChange())
				out <- e
			}()
			waiting := false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				if e := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, int32(peer.PgConn().PID())).Scan(&waiting); e != nil {
					t.Fatal(e)
				}
				if waiting {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !waiting {
				run(t, c, "ROLLBACK")
				t.Fatal("writer did not wait")
			}
			for {
				var expired bool
				if e := c.QueryRow(ctx, `SELECT clock_timestamp()>$1`, expires).Scan(&expired); e != nil {
					t.Fatal(e)
				}
				if expired {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			run(t, c, "COMMIT")
			e := <-out
			want := access.ErrNotFound
			if kind == "membership" {
				want = access.ErrInvalidIdentity
			}
			if !errors.Is(e, want) {
				t.Fatal(kind, e)
			}
			var v int64
			if e := c.QueryRow(ctx, `SELECT version FROM tenant_file_upload_policy WHERE tenant_id=$1`, tenantA).Scan(&v); e != nil || v != 0 {
				t.Fatal(v, e)
			}
		})
	}
}
func TestFileUploadPolicyHistory(t *testing.T) {
	c := filePolicyDB(t)
	svc := access.Service{DB: c}
	ctx := context.Background()
	for i := int64(0); i < 3; i++ {
		v := filePolicyChange()
		v.ExpectedVersion = i
		v.Policy.MaxSizeBytes = 1 + i
		if _, e := svc.SetFileUploadPolicy(ctx, identity(), v); e != nil {
			t.Fatal(e)
		}
	}
	a, e := svc.ListFileUploadPolicyHistory(ctx, identity(), "", 2)
	if e != nil || len(a.History) != 2 || a.History[0].Policy.Version != 3 || a.History[1].Policy.Version != 2 || a.NextCursor == "" {
		t.Fatal(a, e)
	}
	b, e := svc.ListFileUploadPolicyHistory(ctx, identity(), a.NextCursor, 2)
	if e != nil || len(b.History) != 1 || b.History[0].Policy.Version != 1 || b.NextCursor != "" {
		t.Fatal(b, e)
	}
	for _, limit := range []int{0, 101} {
		if _, e := svc.ListFileUploadPolicyHistory(ctx, identity(), "", limit); e == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	foreign := identity()
	foreign.TenantID = tenantB
	if _, e := svc.ListFileUploadPolicyHistory(ctx, foreign, a.NextCursor, 20); e == nil {
		t.Fatal("foreign cursor accepted")
	}
}

func TestFileUploadPolicyOrganizationAdmin(t *testing.T) {
	c := filePolicyDB(t)
	run(t, c, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,scope_organization_id,effective_from) VALUES('00000000-0000-4000-8000-00000000f181',$1,$2,$3,'organization_admin',$3,'2020-01-01')`, tenantA, personM, orgA)
	id := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	svc := access.Service{DB: c}
	if _, e := svc.GetFileUploadPolicy(context.Background(), id); !errors.Is(e, access.ErrNotFound) {
		t.Fatal(e)
	}
	if _, e := svc.SetFileUploadPolicy(context.Background(), id, filePolicyChange()); !errors.Is(e, access.ErrNotFound) {
		t.Fatal(e)
	}
	if _, e := svc.ListFileUploadPolicyHistory(context.Background(), id, "", 20); !errors.Is(e, access.ErrNotFound) {
		t.Fatal(e)
	}
}
