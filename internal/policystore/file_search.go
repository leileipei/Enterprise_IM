package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"strings"
	"time"
)

func (s Service) SearchFileMessages(ctx context.Context, id access.TrustedIdentity, conversationID, kind, query, cursor string, limit int) (FileSearchPage, error) {
	q, e := NormalizeMessageSearchQuery(query)
	if e != nil || limit < 1 || limit > 50 || !directoryUUIDPattern.MatchString(conversationID) || (kind != "direct" && kind != "group") {
		return FileSearchPage{}, ErrInvalidFileSearch
	}
	id, e = fileIdentity(id)
	if e != nil {
		return FileSearchPage{}, ErrForbidden
	}
	conversationID = strings.ToLower(conversationID)
	b := fileSearchBinding{id.TenantID, id.UserID, id.ActingMembershipID, conversationID, kind, q}
	p, e := decodeFileSearchCursor(cursor, b)
	if e != nil {
		return FileSearchPage{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		page, e := s.searchFileMessagesOnce(ctx, id, b, p, limit)
		if e == nil {
			return page, nil
		}
		if !retryFileSearch(ctx, e, attempt) {
			return FileSearchPage{}, publicFileSearchError(e)
		}
	}
}
func retryFileSearch(ctx context.Context, e error, attempt int) bool {
	var p *pgconn.PgError
	return attempt < 2 && ctx.Err() == nil && errors.As(e, &p) && (p.Code == "40P01" || p.Code == "40001" || p.Code == "55P03")
}
func publicFileSearchError(e error) error {
	for _, known := range []error{ErrForbidden, ErrMessageNotAvailable, ErrInvalidFileSearch, ErrAuditUnavailable} {
		if errors.Is(e, known) {
			return e
		}
	}
	return errors.Join(ErrFileSearchUnavailable, e)
}
func (s Service) beginFileSearch(ctx context.Context) (pgx.Tx, error) {
	if s.DB == nil {
		return nil, ErrFileSearchUnavailable
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('statement_timeout','4s',true),set_config('lock_timeout','1s',true)`); e != nil {
		rollbackFileSearch(tx)
		return nil, e
	}
	return tx, nil
}
func rollbackFileSearch(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func auditFileSearch(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, b fileSearchBinding, outcome, reason string, at time.Time) error {
	action, resource, rid := "file_name_search", "conversation", b.Conversation
	if rid == "" {
		action, resource, rid = "file_name_search_all", "tenant", id.TenantID
	}
	_, e := tx.Exec(ctx, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id.TenantID, id.UserID, id.ActingMembershipID, action, resource, rid, outcome, reason, at)
	if e != nil {
		return errors.Join(ErrAuditUnavailable, e)
	}
	return nil
}
func denyFileSearch(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, b fileSearchBinding, reason string, at time.Time, cause error) (FileSearchPage, error) {
	if e := auditFileSearch(ctx, tx, id, b, "deny", reason, at); e != nil {
		return FileSearchPage{}, e
	}
	if e := tx.Commit(ctx); e != nil {
		return FileSearchPage{}, e
	}
	return FileSearchPage{}, cause
}
func (s Service) searchFileMessagesOnce(ctx context.Context, id access.TrustedIdentity, b fileSearchBinding, p fileSearchPosition, limit int) (FileSearchPage, error) {
	tx, e := s.beginFileSearch(ctx)
	if e != nil {
		return FileSearchPage{}, e
	}
	defer rollbackFileSearch(tx)
	var accessible bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.tenant_id=$1 AND c.id=$2 AND c.kind=$3 AND ((c.kind='direct' AND (c.direct_user_low_id=$4 OR c.direct_user_high_id=$4)) OR (c.kind='group' AND EXISTS(SELECT 1 FROM conversation_membership_intervals i WHERE i.tenant_id=c.tenant_id AND i.conversation_id=c.id AND i.user_id=$4))))`, id.TenantID, b.Conversation, b.Kind, id.UserID).Scan(&accessible)
	if e != nil {
		return FileSearchPage{}, e
	}
	batches := []fileSearchBatch{{conversation: b.Conversation, kind: b.Kind}}
	if accessible {
		batches[0], e = readFileSearchCandidatesTx(ctx, tx, id, historyCandidate{b.Conversation, b.Kind}, p.After, 500)
		if e != nil {
			return FileSearchPage{}, e
		}
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
	// No authorization I/O follows this clock.
	at, e := fileClock(ctx, tx)
	if e != nil {
		return FileSearchPage{}, e
	}
	if !found || !memberActiveAt(actor, at) {
		return denyFileSearch(ctx, tx, id, b, "invalid_identity", at, ErrForbidden)
	}
	if !accessible {
		return denyFileSearch(ctx, tx, id, b, "not_available", at, ErrMessageNotAvailable)
	}
	page := FileSearchPage{Matches: make([]FileSearchMatch, 0, limit), HasMore: batches[0].more}
	after := p.After
	for i, c := range batches[0].candidates {
		after = c.seq
		match, visible, e := fileSearchVisibleMatch(c, staged[c.message], at, b.Query)
		if e != nil {
			return FileSearchPage{}, e
		}
		if visible {
			page.Matches = append(page.Matches, match)
		}
		if len(page.Matches) == limit {
			page.HasMore = page.HasMore || i+1 < len(batches[0].candidates)
			break
		}
	}
	if page.HasMore {
		page.NextCursor, e = encodeFileSearchCursor(b, fileSearchPosition{b.Conversation, "within", after})
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
func loadFileSearchProofs(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, batches []fileSearchBatch, owners map[string]string) (map[string]fileVisibilityFacts, error) {
	staged := map[string]fileVisibilityFacts{}
	for _, b := range batches {
		for _, c := range b.candidates {
			if c.file == "" || owners[c.file] != c.conversation {
				continue
			}
			f, e := loadFileVisibilityFactsTx(ctx, tx, id, c.file)
			if errors.Is(e, filedownload.ErrNotFound) {
				continue
			}
			if errors.Is(e, filedownload.ErrInvalidIdentity) {
				return nil, ErrForbidden
			}
			if e != nil {
				return nil, e
			}
			if f.messageID == c.message && f.seq == c.seq && f.metadata.ConversationID == c.conversation {
				staged[c.message] = f
			}
		}
	}
	return staged, nil
}
func fileSearchVisibleMatch(c fileSearchCandidate, f fileVisibilityFacts, at time.Time, q string) (FileSearchMatch, bool, error) {
	if f.messageID == "" {
		return FileSearchMatch{}, false, nil
	}
	m, mid, seq, e := evaluateFileVisibility(f, at)
	if errors.Is(e, filedownload.ErrInvalidIdentity) {
		return FileSearchMatch{}, false, ErrForbidden
	}
	if errors.Is(e, filedownload.ErrNotFound) {
		return FileSearchMatch{}, false, nil
	}
	if e != nil {
		return FileSearchMatch{}, false, e
	}
	if !strings.Contains(strings.ToLower(m.OriginalFilename), q) {
		return FileSearchMatch{}, false, nil
	}
	return FileSearchMatch{ConversationID: c.conversation, Kind: c.kind, MessageID: mid, SenderUserID: c.sender, FileID: m.ID, OriginalFilename: m.OriginalFilename, DetectedMediaType: m.DetectedMediaType, Seq: seq, ActualSizeBytes: *m.ActualSizeBytes, ServerTime: c.at}, true, nil
}
