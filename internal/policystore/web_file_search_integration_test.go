package policystore_test

import (
	"context"
	"encoding/json"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/url"
	"testing"
	"time"
)

func TestWebFileRealSearch(t *testing.T) {
	f := newWebFileFixture(t)
	group := f.group(t)
	dm := f.uploadReady(t, directA, "\uFEFF集团_ABC%_.txt", []byte("P425_PRIVATE_BODY"))
	f.sendFile(t, directA, "direct", dm.ID, "P425_PRIVATE_CAPTION")
	gm := f.uploadReady(t, group, "集团_ABC%_群.txt", []byte("P425_PRIVATE_BODY"))
	f.sendFile(t, group, "group", gm.ID, "P425_PRIVATE_CAPTION")
	f.private = append(f.private, "P425_PRIVATE_BODY", "P425_PRIVATE_CAPTION")
	f.browser(t, "file_search", map[string]any{"conversation": directA, "group": group, "names": []string{dm.OriginalFilename, gm.OriginalFilename}})
	for _, q := range []string{"P425_PRIVATE_BODY", "P425_PRIVATE_CAPTION"} {
		var p policystore.FileSearchPage
		if e := json.Unmarshal(f.request(t, "GET", "/api/v1/files/search?q="+url.QueryEscape(q), nil, 200), &p); e != nil || len(p.Matches) != 0 {
			t.Fatal("content matched filename search")
		}
	}
	peer := f.peerToken(t)
	expect := func(want int) {
		t.Helper()
		var p struct {
			Matches []any `json:"matches"`
		}
		b := f.requestAs(t, peer, targetM2, "GET", "/api/v1/groups/"+group+"/files/search?q=ABC%25_", nil, 200)
		if e := json.Unmarshal(b, &p); e != nil || len(p.Matches) != want {
			t.Fatal("name qualification mismatch", len(p.Matches), want, e)
		}
	}
	expect(1)
	run(t, f.real.conn, "UPDATE user_organizations SET status='suspended' WHERE id=$1", adminM)
	expect(0)
	run(t, f.real.conn, "UPDATE user_organizations SET status='active' WHERE id=$1", adminM)
	run(t, f.real.conn, "ALTER TABLE message_attachments DISABLE TRIGGER USER")
	run(t, f.real.conn, "UPDATE message_attachments SET sealed_sha256=decode(repeat('ab',32),'hex') WHERE file_id=$1", gm.ID)
	expect(0)
	run(t, f.real.conn, "UPDATE message_attachments SET sealed_sha256=$2 WHERE file_id=$1", gm.ID, gm.SHA256)
	run(t, f.real.conn, "ALTER TABLE message_attachments ENABLE TRIGGER USER")
	member, e := f.real.repo.GetOwnGroupMembership(context.Background(), groupMemberIdentity(), group)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.real.repo.LeaveGroup(context.Background(), groupMemberIdentity(), group, member.IntervalID); e != nil {
		t.Fatal(e)
	}
	expect(0)
	gap := f.uploadReady(t, group, "离群_ABC%_.txt", []byte("gap"))
	f.sendFile(t, group, "group", gap.ID, "")
	if _, e = f.real.repo.InviteGroupMember(context.Background(), publisher(), group, inviteGroupRequest(targetM2)); e != nil {
		t.Fatal(e)
	}
	expect(1) // The original authorized interval remains readable; the gap does not.
	foreign := f.real.issueToken("p425-foreign")
	run(t, f.real.conn, `INSERT INTO external_identities(issuer,subject,tenant_id,user_id) VALUES($1,'p425-foreign',$2,$3)`, f.real.issuer, tenantB, personB)
	f.private = append(f.private, foreign)
	f.requestAs(t, foreign, otherM, "GET", "/api/v1/groups/"+group+"/files/search?q=ABC%25_", nil, 404)
	gm = fileNext(gm, "delete_pending")
	if e = writeFile(f.real.conn, gm, false); e != nil {
		t.Fatal(e)
	}
	expect(0)
	gm = fileNext(gm, "deleted")
	if e = writeFile(f.real.conn, gm, false); e != nil {
		t.Fatal(e)
	}
	expect(0)
	f.assertPrivate(t)
	t.Run("500 normal ready candidates", TestFileSearchReadyCandidateBudget)
	t.Run("20 normal populated conversations", TestCrossFileSearchReadyCandidateBudget)
	t.Run("500 candidates", TestFileSearchBoundedProgress)
	t.Run("20 conversations", TestCrossFileSearchBudget)
	t.Run("no duplicate cursors", TestCrossFileSearchCursorProgress)
}
func TestWebFileRealSearchFinalBoundary(t *testing.T) {
	for _, change := range []string{"future deny", "TTL"} {
		t.Run(change, func(t *testing.T) {
			f := newWebFileFixture(t)
			cid := f.group(t)
			m := f.uploadReady(t, cid, "最终边界.txt", []byte("boundary"))
			f.sendFile(t, cid, "group", m.ID, "")
			ctx := context.Background()
			var boundary time.Time
			if e := f.real.conn.QueryRow(ctx, "SELECT clock_timestamp()+interval '600 milliseconds'").Scan(&boundary); e != nil {
				t.Fatal(e)
			}
			if change == "TTL" {
				setDownloadRetention(t, f.real.conn, 1)
				run(t, f.real.conn, "UPDATE messages SET accepted_at=$2::timestamptz-interval '1 day' WHERE conversation_id=$1", cid, boundary)
			} else {
				r := policy.Rule{ID: "p425-final", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: groupMemberC, EffectiveFrom: boundary, Reason: "scheduled"}
				if _, e := f.real.repo.Publish(ctx, publisher(), 0, []policy.Rule{r}, "approved"); e != nil {
					t.Fatal(e)
				}
			}
			s := f.real.repo
			s.DB = fileSearchLateDB{f.real.conn, boundary}
			p, e := s.SearchAllFileMessages(ctx, groupMemberIdentity(), m.OriginalFilename, "group", "", 20)
			if e != nil || len(p.Matches) != 0 {
				t.Fatal("final clock leaked a name", e)
			}
			var sessions int
			if e = f.real.conn.QueryRow(ctx, "SELECT count(*) FROM file_download_sessions").Scan(&sessions); e != nil || sessions != 0 {
				t.Fatal("search allocated download session", sessions, e)
			}
			f.assertPrivate(t)
		})
	}
	t.Run("protected final clock wait", TestFileSearchConcurrencyFinalClockWait)
	t.Run("audit atomic", TestFileSearchAuditFailure)
}
