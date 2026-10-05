package policystore

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"time"
)

// Personal candidate history only. All returned names share one final-time proof.
func (s Service) SearchAllFileMessages(ctx context.Context, id access.TrustedIdentity, query, kind, cursor string, limit int) (FileSearchPage, error) {
	q, e := NormalizeMessageSearchQuery(query)
	if e != nil || limit < 1 || limit > 50 || (kind != "all" && kind != "direct" && kind != "group") {
		return FileSearchPage{}, ErrInvalidFileSearch
	}
	id, e = fileIdentity(id)
	if e != nil {
		return FileSearchPage{}, ErrForbidden
	}
	b := fileSearchBinding{Tenant: id.TenantID, User: id.UserID, Membership: id.ActingMembershipID, Kind: kind, Query: q}
	p, e := decodeFileSearchCursor(cursor, b)
	if e != nil {
		return FileSearchPage{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		page, e := s.searchAllFileMessagesOnce(ctx, id, b, p, limit)
		if e == nil {
			return page, nil
		}
		if !retryFileSearch(ctx, e, attempt) {
			return FileSearchPage{}, publicFileSearchError(e)
		}
	}
}
func (s Service) searchAllFileMessagesOnce(ctx context.Context, id access.TrustedIdentity, b fileSearchBinding, p fileSearchPosition, limit int) (FileSearchPage, error) {
	tx, e := s.beginFileSearch(ctx)
	if e != nil {
		return FileSearchPage{}, e
	}
	defer rollbackFileSearch(tx)
	candidates, e := listCrossSearchCandidatesTx(ctx, tx, id, b.Kind, crossSearchPosition{p.Conversation, p.Phase, p.After})
	if e != nil {
		return FileSearchPage{}, e
	}
	batches := make([]fileSearchBatch, 0, min(20, len(candidates)))
	remaining := 500
	more := false
	for i, c := range candidates {
		if i == 20 || remaining == 0 {
			more = true
			break
		}
		after := int64(0)
		if p.Conversation == c.ID && p.Phase == "within" {
			after = p.After
		}
		batch, e := readFileSearchCandidatesTx(ctx, tx, id, c, after, remaining)
		if e != nil {
			return FileSearchPage{}, e
		}
		remaining -= len(batch.candidates)
		batches = append(batches, batch)
	}
	owners, e := prelockFileSearchTx(ctx, tx, id, batches)
	if e != nil {
		return FileSearchPage{}, e
	}
	actor, found, e := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if e != nil {
		return FileSearchPage{}, e
	}
	staged, e := loadFileSearchProofs(ctx, tx, id, batches, owners)
	if e != nil {
		return FileSearchPage{}, e
	}
	at, e := fileClock(ctx, tx)
	if e != nil {
		return FileSearchPage{}, e
	}
	if !found || !memberActiveAt(actor, at) {
		return denyFileSearch(ctx, tx, id, b, "invalid_identity", at, ErrForbidden)
	}
	// From this point forward only value facts are evaluated; no authorization reads.
	page := FileSearchPage{Matches: make([]FileSearchMatch, 0, limit), HasMore: more}
	progress := p
	stopped := false
	for i, batch := range batches {
		for j, c := range batch.candidates {
			progress = fileSearchPosition{batch.conversation, "within", c.seq}
			match, visible, e := fileSearchVisibleMatch(c, staged[c.message], at, b.Query)
			if e != nil {
				return FileSearchPage{}, e
			}
			if visible {
				page.Matches = append(page.Matches, match)
			}
			if len(page.Matches) == limit {
				page.HasMore = more || batch.more || j+1 < len(batch.candidates) || i+1 < len(batches)
				stopped = true
				break
			}
		}
		if stopped {
			break
		}
		if batch.more {
			page.HasMore = true
			break
		}
		progress = fileSearchPosition{batch.conversation, "after", 0}
	}
	if page.HasMore {
		page.NextCursor, e = encodeFileSearchCursor(b, progress)
		if e != nil {
			return FileSearchPage{}, e
		}
	}
	if e = auditFileSearch(ctx, tx, id, b, "allow", "completed", at); e != nil {
		return FileSearchPage{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return FileSearchPage{}, e
	}
	return page, nil
}
