package policystore_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
	"time"
)

func downloadAuthorize(t *testing.T, c *pgx.Conn, id access.TrustedIdentity, fid string) error {
	t.Helper()
	ctx := context.Background()
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	m, mid, seq, fresh, e := policystore.FileDownloadAuthorizationForTest(ctx, tx, id, fid)
	if e == nil && (m.ID != fid || mid == "" || seq < 1 || fresh.IsZero()) {
		t.Fatal("incomplete evidence", m, mid, seq, fresh)
	}
	return e
}
func TestFileDownloadAuthorizationSources(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			c, _, _, m := fileHistoryFixture(t, kind)
			if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); e != nil {
				t.Fatal("valid recipient", e)
			}
			if e := downloadAuthorize(t, c, publisher(), m.ID); e != nil {
				t.Fatal("valid uploader", e)
			}
			foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
			if e := downloadAuthorize(t, c, foreign, m.ID); !errors.Is(e, filedownload.ErrNotFound) {
				t.Fatal("foreign", e)
			}
			unbound := fileMessageFixture(t, c, m.ConversationID, adminA, adminM)
			if e := downloadAuthorize(t, c, publisher(), unbound.ID); !errors.Is(e, filedownload.ErrNotFound) {
				t.Fatal("unbound", e)
			}
			if e := downloadAuthorize(t, c, publisher(), freshFile().ID); !errors.Is(e, filedownload.ErrNotFound) {
				t.Fatal("missing", e)
			}
			var n int
			if e := c.QueryRow(context.Background(), "SELECT count(*) FROM file_download_sessions").Scan(&n); e != nil || n != 0 {
				t.Fatal("authorization wrote sessions", n, e)
			}
		})
	}
}
func TestFileDownloadAuthorizationMembershipGap(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "group")
	run(t, c, "UPDATE conversation_membership_intervals SET status='left',leave_seq=1,left_at=$3 WHERE conversation_id=$1 AND user_id=$2", cid, personA, at)
	if _, e := s.InviteGroupMember(context.Background(), publisher(), cid, inviteGroupRequest(targetM2)); e != nil {
		t.Fatal(e)
	}
	if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("rejoin gap", e)
	}
}
func TestFileDownloadAuthorizationPolicies(t *testing.T) {
	for _, action := range []policy.Action{policy.ActionSendMessage, policy.ActionFileDownload} {
		for _, effect := range []policy.Effect{policy.EffectHardDeny, policy.EffectIsolate} {
			t.Run(string(action)+"/"+string(effect), func(t *testing.T) {
				c, s, _, m := fileHistoryFixture(t, "direct")
				grantPublisher(t, c)
				r := policy.Rule{ID: "download-deny", TenantID: tenantA, Action: action, Effect: effect, SourceMembershipID: targetM2, TargetMembershipID: adminM, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Hour), Reason: "block"}
				if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{r}, "download"); e != nil {
					t.Fatal(e)
				}
				if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); !errors.Is(e, filedownload.ErrNotFound) {
					t.Fatal("deny", e)
				}
			})
		}
	}
	// Reverse hard deny is decisive even when the rule is one-directional.
	c, s, _, m := fileHistoryFixture(t, "direct")
	grantPublisher(t, c)
	r := policy.Rule{ID: "reverse", TenantID: tenantA, Action: policy.ActionFileDownload, Effect: policy.EffectHardDeny, SourceMembershipID: adminM, TargetMembershipID: targetM2, SourceOrganizationID: orgA, TargetOrganizationID: orgA, EffectiveFrom: at.Add(-time.Hour), Reason: "reverse"}
	if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{r}, "reverse"); e != nil {
		t.Fatal(e)
	}
	if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal(e)
	}
}
func TestFileDownloadAuthorizationUploaderInactive(t *testing.T) {
	for _, which := range []string{"account", "membership", "expired"} {
		t.Run(which, func(t *testing.T) {
			c, _, _, m := fileHistoryFixture(t, "group")
			switch which {
			case "account":
				run(t, c, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
			case "membership":
				run(t, c, "UPDATE user_organizations SET status='suspended' WHERE id=$1", adminM)
			case "expired":
				run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp()-interval '1 second' WHERE id=$1", adminM)
			}
			if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); !errors.Is(e, filedownload.ErrNotFound) {
				t.Fatal(e)
			}
		})
	}
}
func TestFileDownloadAuthorizationAdminNoBypass(t *testing.T) {
	for _, which := range []string{"policy_blocked", "ended", "source_changed", "requester_inactive"} {
		t.Run(which, func(t *testing.T) {
			c, _, cid, m := fileHistoryFixture(t, "direct")
			id := publisher()
			want := filedownload.ErrNotFound
			switch which {
			case "source_changed":
				otherAdminM := freshFile().ID
				run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", otherAdminM, tenantA, adminA, orgA2)
				run(t, c, "UPDATE conversations SET direct_low_membership_id=$2 WHERE id=$1", cid, otherAdminM)
			case "requester_inactive":
				run(t, c, "UPDATE users SET status='frozen' WHERE id=$1", adminA)
				want = filedownload.ErrInvalidIdentity
			default:
				run(t, c, "UPDATE conversations SET status=$2 WHERE id=$1", cid, which)
			}
			if e := downloadAuthorize(t, c, id, m.ID); !errors.Is(e, want) {
				t.Fatal(e)
			}
		})
	}
}
func setDownloadRetention(t *testing.T, c *pgx.Conn, days int64) {
	t.Helper()
	run(t, c, `UPDATE tenant_file_retention_policy SET file_retention_days=$2,version=version+1,approval_reference='test',actor_user_id=$3,acting_membership_id=$4,updated_at=clock_timestamp() WHERE tenant_id=$1`, tenantA, days, adminA, adminM)
}
func TestFileDownloadAuthorizationDynamicRetention(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		for _, which := range []string{"file_expired", "body_expired", "cleared", "pending"} {
			t.Run(kind+"/"+which, func(t *testing.T) {
				c, _, cid, m := fileHistoryFixture(t, kind)
				if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); e != nil {
					t.Fatal(e)
				}
				switch which {
				case "file_expired":
					setDownloadRetention(t, c, 1)
				case "body_expired":
					run(t, c, `UPDATE tenants SET message_body_retention_days=1,retention_version=1,retention_approval_reference='test',retention_approved_by_user_id=$2,retention_approved_at=clock_timestamp() WHERE id=$1`, tenantA, adminA)
				case "cleared":
					run(t, c, "UPDATE messages SET text_body=NULL,body_cleared_at=clock_timestamp() WHERE conversation_id=$1 AND seq=2", cid)
				case "pending":
					m = fileNext(m, "delete_pending")
					if e := writeFile(c, m, false); e != nil {
						t.Fatal(e)
					}
				}
				if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); !errors.Is(e, filedownload.ErrNotFound) {
					t.Fatal(which, e)
				}
				setDownloadRetention(t, c, 3650)
				e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID)
				if which == "file_expired" {
					if e != nil {
						t.Fatal("ready extension", e)
					}
				} else if !errors.Is(e, filedownload.ErrNotFound) {
					t.Fatal("revived", e)
				}
			})
		}
	}
}

func TestFileDownloadAuthorizationCrossOrganization(t *testing.T) {
	c, s, m, req := directFileFixture(t)
	grantPublisher(t, c)
	run(t, c, "UPDATE conversations SET direct_high_membership_id=$2 WHERE id=$1", directA, targetM)
	send := policy.Rule{ID: "send-allow", TenantID: tenantA, Effect: policy.EffectAllow, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA2, Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(30 * 24 * time.Hour), Reason: "explicit"}
	if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{send}, "cross"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SendMessage(context.Background(), publisher(), directA, req); e != nil {
		t.Fatal(e)
	}
	id := groupMemberIdentity()
	id.ActingMembershipID = targetM
	if e := downloadAuthorize(t, c, id, m.ID); !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("send allow copied to file", e)
	}
	download := send
	download.ID = "download-allow"
	download.Action = policy.ActionFileDownload
	if _, e := s.Publish(context.Background(), publisher(), 1, []policy.Rule{send, download}, "explicit file"); e != nil {
		t.Fatal(e)
	}
	if e := downloadAuthorize(t, c, id, m.ID); e != nil {
		t.Fatal("explicit file allow", e)
	}
}
func TestFileDownloadAuthorizationScanMismatch(t *testing.T) {
	c, _, _, m := fileHistoryFixture(t, "direct")
	// Inject damaged persisted evidence only in this owned test schema. The normal
	// schema blocks this corruption; the reader must also reject damaged snapshots.
	rows, e := c.Query(context.Background(), `SELECT conname FROM pg_constraint WHERE conrelid='file_objects'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%scan_sha256%'`)
	if e != nil {
		t.Fatal(e)
	}
	var names []string
	for rows.Next() {
		var n string
		if e = rows.Scan(&n); e != nil {
			t.Fatal(e)
		}
		names = append(names, n)
	}
	e = rows.Err()
	rows.Close()
	if e != nil || len(names) == 0 {
		t.Fatal(names, e)
	}
	for _, n := range names {
		run(t, c, "ALTER TABLE file_objects DROP CONSTRAINT "+pgx.Identifier{n}.Sanitize())
	}
	run(t, c, "ALTER TABLE file_objects DISABLE TRIGGER USER")
	run(t, c, "UPDATE file_objects SET scan_sha256=decode(repeat('cd',32),'hex') WHERE id=$1", m.ID)
	if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("damaged scan authorized", e)
	}
}

func TestFileDownloadAuthorizationDoesNotRequireAllGroupPairs(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "group")
	grantPublisher(t, c)
	r := policy.Rule{ID: "other-pair-isolated", TenantID: tenantA, Effect: policy.EffectIsolate, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, Bidirectional: true, EffectiveFrom: at.Add(-time.Hour), Reason: "other pair"}
	if _, e := s.Publish(context.Background(), publisher(), 0, []policy.Rule{r}, "other pair"); e != nil {
		t.Fatal(e)
	}
	if e := downloadAuthorize(t, c, groupMemberIdentity(), m.ID); e != nil {
		t.Fatal("download requires unrelated pair", e)
	}
	var messages, attachments, outbox, idem, seq int64
	if e := c.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM messages WHERE conversation_id=$1),(SELECT count(*) FROM message_attachments WHERE conversation_id=$1),(SELECT count(*) FROM outbox_events WHERE conversation_id=$1),(SELECT count(*) FROM message_idempotency WHERE conversation_id=$1),(SELECT last_seq FROM conversations WHERE id=$1)`, cid).Scan(&messages, &attachments, &outbox, &idem, &seq); e != nil || messages != 3 || attachments != 1 || outbox != 3 || idem != 3 || seq != 3 {
		t.Fatal(messages, attachments, outbox, idem, seq, e)
	}
}

type delayedDownloadParticipationTx struct {
	pgx.Tx
	expires time.Time
}

func (d delayedDownloadParticipationTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	if strings.Contains(q, "SELECT EXISTS(SELECT 1 FROM conversation_membership_intervals") {
		d.Tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, d.expires)
	}
	return d.Tx.QueryRow(ctx, q, args...)
}
func TestFileDownloadAuthorizationExpiryAfterParticipationQuery(t *testing.T) {
	c, _, _, m := fileHistoryFixture(t, "group")
	ctx := context.Background()
	var expires time.Time
	if e := c.QueryRow(ctx, "SELECT clock_timestamp()+interval '1 second'").Scan(&expires); e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", targetM2, expires)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	_, _, _, _, e = policystore.FileDownloadAuthorizationForTest(ctx, delayedDownloadParticipationTx{Tx: tx, expires: expires}, groupMemberIdentity(), m.ID)
	if !errors.Is(e, filedownload.ErrInvalidIdentity) {
		t.Fatal("identity expired after late query", e)
	}
}

func TestFileDownloadAuthorizationLateGroupHistoryHardDeny(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "group")
	grantPublisher(t, c)
	ctx := context.Background()
	var begins time.Time
	if e := c.QueryRow(ctx, "SELECT clock_timestamp()+interval '1 second'").Scan(&begins); e != nil {
		t.Fatal(e)
	}
	r := policy.Rule{ID: "late-group-history", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, EffectiveFrom: begins, Reason: "future history block"}
	if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{r}, "late"); e != nil {
		t.Fatal(e)
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	_, _, _, _, e = policystore.FileDownloadAuthorizationForTest(ctx, delayedDownloadParticipationTx{Tx: tx, expires: begins}, groupMemberIdentity(), m.ID)
	if !errors.Is(e, filedownload.ErrNotFound) {
		t.Fatal("late group history deny", e)
	}
}
