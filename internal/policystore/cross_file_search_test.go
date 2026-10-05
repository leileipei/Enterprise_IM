package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"sort"
	"testing"
)

func TestCrossFileSearchPersonalScope(t *testing.T) {
	c, s, _, m := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	g, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2))
	if e != nil {
		t.Fatal(e)
	}
	gm := namedSearchFile(t, c, g.ID, "集团中文报告.pdf")
	if _, e = s.SendGroupMessage(ctx, publisher(), g.ID, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 14100), MessageType: "file", FileID: gm.ID}); e != nil {
		t.Fatal(e)
	}
	p, e := s.SearchAllFileMessages(ctx, groupMemberIdentity(), ".PDF", "all", "", 20)
	if e != nil || len(p.Matches) != 2 || p.HasMore {
		t.Fatal(p, e)
	}
	if p.Matches[0].ConversationID >= p.Matches[1].ConversationID {
		t.Fatal("UUID order", p)
	}
	found := map[string]bool{}
	for _, v := range p.Matches {
		found[v.FileID] = true
	}
	if !found[m.ID] || !found[gm.ID] {
		t.Fatal("lost scope", p)
	}
	foreign := access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}
	p, e = s.SearchAllFileMessages(ctx, foreign, ".pdf", "all", "", 20)
	if e != nil || len(p.Matches) != 0 {
		t.Fatal("foreign leak", p, e)
	}
	run(t, c, "UPDATE user_organizations SET status='suspended' WHERE id=$1", adminM)
	p, e = s.SearchAllFileMessages(ctx, groupMemberIdentity(), ".pdf", "all", "", 20)
	if e != nil || len(p.Matches) != 0 {
		t.Fatal("inactive uploader leak", p, e)
	}
}
func TestCrossFileSearchBudget(t *testing.T) {
	c, s, _, _ := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	var ids []string
	for i := 0; i < 21; i++ {
		r := createGroupRequest(targetM2)
		r.ClientRequestID = clientUUIDv7(at, 14500+i)
		g, e := s.CreateGroup(ctx, publisher(), r)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, g.ID)
	}
	sort.Strings(ids)
	m := namedSearchFile(t, c, ids[20], "needle.pdf")
	if _, e := s.SendGroupMessage(ctx, publisher(), ids[20], policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 14600), MessageType: "file", FileID: m.ID}); e != nil {
		t.Fatal(e)
	}
	p, e := s.SearchAllFileMessages(ctx, publisher(), "needle", "group", "", 20)
	if e != nil || len(p.Matches) != 0 || !p.HasMore || p.NextCursor == "" {
		t.Fatal("20 boundary", p, e)
	}
	p, e = s.SearchAllFileMessages(ctx, publisher(), "needle", "group", p.NextCursor, 20)
	if e != nil || len(p.Matches) != 1 || p.Matches[0].FileID != m.ID || p.HasMore {
		t.Fatal("21st skipped", p, e)
	}
}
func TestCrossFileSearchCursorProgress(t *testing.T) {
	c, s, cid, _ := fileHistoryFixture(t, "direct")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		m := namedSearchFile(t, c, cid, "报告.pdf")
		if _, e := s.SendMessage(ctx, publisher(), cid, policystore.MessageSendRequest{ClientMessageID: clientUUIDv7(at, 14700+i), MessageType: "file", FileID: m.ID}); e != nil {
			t.Fatal(e)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 3; i++ {
		p, e := s.SearchAllFileMessages(ctx, publisher(), ".pdf", "direct", cursor, 1)
		if e != nil || len(p.Matches) != 1 || seen[p.Matches[0].MessageID] {
			t.Fatal(p, e)
		}
		seen[p.Matches[0].MessageID] = true
		cursor = p.NextCursor
		if i < 2 && (!p.HasMore || cursor == "") {
			t.Fatal("lost page", p)
		}
		if i == 2 && p.HasMore {
			t.Fatal("invented page", p)
		}
	}
	p, e := s.SearchAllFileMessages(ctx, publisher(), ".pdf", "direct", "", 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SearchAllFileMessages(ctx, publisher(), "different", "direct", p.NextCursor, 1); !errors.Is(e, policystore.ErrInvalidFileSearch) {
		t.Fatal("cursor reused", e)
	}
}
