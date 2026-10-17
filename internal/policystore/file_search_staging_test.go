package policystore_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

// With one prepared proof, its early clock and provisional clock precede the
// final clock. With two initially denied proofs, the fifth clock is final.
// Delay that boundary in PostgreSQL, rather than guessing a host-side sleep.
type fileSearchStageClockDB struct {
	c           *pgx.Conn
	t           *testing.T
	boundary    time.Time
	finalRead   int
	temporaryAt *time.Time
	waited      *bool
}

func (d fileSearchStageClockDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.c.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return &fileSearchStageClockTx{Tx: tx, db: d}, nil
}

type fileSearchStageClockTx struct {
	pgx.Tx
	db     fileSearchStageClockDB
	clocks int
}

func (d *fileSearchStageClockTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	if q == "SELECT clock_timestamp()" {
		d.clocks++
		if d.clocks == d.db.finalRead {
			*d.db.waited = true
			if _, e := d.Tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, d.db.boundary); e != nil {
				d.db.t.Fatal("controlled final boundary", e)
			}
		}
	}
	row := d.Tx.QueryRow(ctx, q, args...)
	if q == "SELECT clock_timestamp()" && d.clocks == 2 && d.db.temporaryAt.IsZero() {
		return fileSearchStageClockRow{Row: row, observed: d.db.temporaryAt}
	}
	return row
}

type fileSearchStageClockRow struct {
	pgx.Row
	observed *time.Time
}

func (r fileSearchStageClockRow) Scan(dest ...any) error {
	e := r.Row.Scan(dest...)
	if e == nil {
		*r.observed = *(dest[0].(*time.Time))
	}
	return e
}

func TestFileSearchStagingFinalTimeCursor(t *testing.T) {
	for _, cross := range []bool{false, true} {
		for _, change := range []string{"expires", "deny_ends"} {
			t.Run(map[bool]string{false: "single", true: "cross"}[cross]+"/"+change, func(t *testing.T) {
				kind := "direct"
				if cross {
					kind = "group"
				}
				c, s, cid, first := fileHistoryFixture(t, kind)
				ctx := context.Background()
				second := namedSearchFile(t, c, cid, first.OriginalFilename)
				req := policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 24501), MessageType: "file", FileID: second.ID}
				send := s.SendMessage
				if cross {
					send = s.SendGroupMessage
				}
				if _, e := send(ctx, publisher(), cid, req); e != nil {
					t.Fatal(e)
				}
				var boundary time.Time
				if e := c.QueryRow(ctx, "SELECT clock_timestamp()+interval '500 milliseconds'").Scan(&boundary); e != nil {
					t.Fatal(e)
				}
				finalRead := 3
				if change == "expires" {
					setDownloadRetention(t, c, 1)
					run(t, c, "UPDATE messages SET accepted_at=CASE WHEN seq=2 THEN $2::timestamptz-interval '1 day' ELSE clock_timestamp() END WHERE conversation_id=$1 AND message_type='file'", cid, boundary)
				} else {
					grantPublisher(t, c)
					r := policy.Rule{ID: "staging-deny-ends", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionFileDownload, SourceOrganizationID: orgA, TargetOrganizationID: orgA, SourceMembershipID: targetM2, TargetMembershipID: adminM, EffectiveFrom: boundary.Add(-time.Hour), EffectiveTo: boundary, Reason: "test"}
					if _, e := s.Publish(ctx, publisher(), 0, []policy.Rule{r}, "test"); e != nil {
						t.Fatal(e)
					}
					finalRead = 5
				}
				var temporaryAt time.Time
				waited := false
				s.DB = fileSearchStageClockDB{c: c, t: t, boundary: boundary, finalRead: finalRead, temporaryAt: &temporaryAt, waited: &waited}
				cursor := ""
				seen := map[string]bool{}
				finished := false
				for page := 0; page < 4; page++ {
					var p policystore.FileSearchPage
					var e error
					if cross {
						p, e = s.SearchAllFileMessages(ctx, groupMemberIdentity(), first.OriginalFilename, kind, cursor, 1)
					} else {
						p, e = s.SearchFileMessages(ctx, groupMemberIdentity(), cid, kind, first.OriginalFilename, cursor, 1)
					}
					if e != nil {
						t.Fatal(e)
					}
					if page == 0 && (!waited || temporaryAt.IsZero() || !temporaryAt.Before(boundary)) {
						t.Fatal("controlled pre-final clock boundary was not exercised", waited, temporaryAt, boundary)
					}
					for _, m := range p.Matches {
						if seen[m.FileID] {
							t.Fatal("duplicate after staging boundary", m.FileID)
						}
						seen[m.FileID] = true
					}
					if !p.HasMore {
						finished = true
						break
					}
					if p.NextCursor == "" || p.NextCursor == cursor {
						t.Fatal("cursor did not advance", p)
					}
					cursor = p.NextCursor
				}
				if !finished || !seen[second.ID] || change == "expires" && seen[first.ID] || change == "deny_ends" && !seen[first.ID] {
					t.Fatal("final-time staging lost or exposed a file", finished, seen)
				}
			})
		}
	}
}

func TestFileSearchStagingDeletedName(t *testing.T) {
	c, s, cid, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	cleanupPolicy(t, c, 1, true)
	run(t, c, "UPDATE messages SET accepted_at=clock_timestamp()-interval '2 days' WHERE conversation_id=$1 AND message_type='file'", cid)
	ticket := claimDelete(t, s)
	if e := s.RecordFileDeleteInventory(ctx, ticket, filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: m.ObjectVersionID}}, Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	permit := commitDelete(t, s, ticket, m.ObjectVersionID)
	proofAt, e := s.FileDeleteProofTime(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SettleFileDeleteVersion(ctx, permit, filecleanup.AbsenceProof{VersionID: permit.VersionID, Absent: true, CheckedAt: proofAt}); e != nil {
		t.Fatal(e)
	}
	if e = s.RecordFileDeleteInventory(ctx, ticket, filecleanup.Inventory{Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	if e = s.FinalizeFileDelete(ctx, ticket); e != nil {
		t.Fatal(e)
	}
	var absent bool
	if e = c.QueryRow(ctx, "SELECT original_filename IS NULL FROM file_objects WHERE id=$1", m.ID).Scan(&absent); e != nil || !absent {
		t.Fatal("fixture did not clear original name", absent, e)
	}
	p, e := s.SearchFileMessages(ctx, groupMemberIdentity(), cid, "direct", m.OriginalFilename, "", 20)
	if e != nil || len(p.Matches) != 0 {
		t.Fatal("deleted nullable name returned or broke search", p, e)
	}
}

func TestCrossFileSearchStagingConversationEnd(t *testing.T) {
	c, s, _, first := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	g, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	second := namedSearchFile(t, c, g.ID, first.OriginalFilename)
	if _, e = s.SendGroupMessage(ctx, publisher(), g.ID, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 24502), MessageType: "file", FileID: second.ID}); e != nil {
		t.Fatal(e)
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 2; i++ {
		p, e := s.SearchAllFileMessages(ctx, groupMemberIdentity(), first.OriginalFilename, "all", cursor, 1)
		if e != nil || len(p.Matches) != 1 || seen[p.Matches[0].FileID] {
			t.Fatal("conversation boundary missing or duplicate", p, e)
		}
		seen[p.Matches[0].FileID] = true
		if (i == 0) != p.HasMore {
			t.Fatal("later conversation lost or invented", p)
		}
		cursor = p.NextCursor
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatal("later conversation was skipped", seen)
	}
}
