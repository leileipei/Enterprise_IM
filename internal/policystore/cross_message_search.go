package policystore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

type CrossConversationMatch struct {
	ConversationID, Kind string
	Message              PulledMessage
}
type CrossConversationSearchPage struct {
	Messages   []CrossConversationMatch
	HasMore    bool
	NextCursor string
}

// SearchAllTextMessages scans personal history, never a tenant-wide text index.
func (s Service) SearchAllTextMessages(ctx context.Context, id access.TrustedIdentity, query, kind, cursor string, limit int) (CrossConversationSearchPage, error) {
	query, err := NormalizeMessageSearchQuery(query)
	if err != nil || limit < 1 || limit > 50 || (kind != "all" && kind != "direct" && kind != "group") {
		return CrossConversationSearchPage{}, ErrInvalidMessageSearch
	}
	if !directoryUUIDPattern.MatchString(id.TenantID) || !directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return CrossConversationSearchPage{}, ErrForbidden
	}
	id.TenantID = strings.ToLower(id.TenantID)
	id.UserID = strings.ToLower(id.UserID)
	id.ActingMembershipID = strings.ToLower(id.ActingMembershipID)
	binding := crossSearchBinding{id.TenantID, id.UserID, id.ActingMembershipID, query, kind}
	position, err := decodeCrossSearchCursor(cursor, binding)
	if err != nil {
		return CrossConversationSearchPage{}, err
	}
	if s.DB == nil {
		return CrossConversationSearchPage{}, errors.New("search store unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		page, err := s.searchAllTextMessagesOnce(ctx, id, binding, position, limit)
		var pgerr *pgconn.PgError
		if err == nil {
			return page, nil
		}
		if attempt >= 2 || ctx.Err() != nil || !errors.As(err, &pgerr) || (pgerr.Code != "40P01" && pgerr.Code != "40001") {
			return CrossConversationSearchPage{}, err
		}
	}
}

func refreshCrossSearchPolicyTx(ctx context.Context, tx pgx.Tx, scope historyReadContext) (historyReadContext, error) {
	var version int64
	err := tx.QueryRow(ctx, `SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE`, scope.Identity.TenantID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		version = 0
	} else if err != nil {
		return historyReadContext{}, err
	}
	rules, err := loadRules(ctx, tx, scope.Identity.TenantID, version)
	if err != nil {
		return historyReadContext{}, err
	}
	scope.Rules = rules
	return scope, nil
}

func auditCrossSearch(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,'message_search_all','tenant',$1,$4,$5,$6)`, id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func (s Service) searchAllTextMessagesOnce(ctx context.Context, id access.TrustedIdentity, binding crossSearchBinding, position crossSearchPosition, limit int) (CrossConversationSearchPage, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return CrossConversationSearchPage{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return CrossConversationSearchPage{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout','4s',true),set_config('lock_timeout','1s',true)`); err != nil {
		return CrossConversationSearchPage{}, err
	}
	deny := func() (CrossConversationSearchPage, error) {
		if err := auditCrossSearch(ctx, tx, id, "deny", "invalid_identity", s.now()); err != nil {
			return CrossConversationSearchPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CrossConversationSearchPage{}, err
		}
		return CrossConversationSearchPage{}, ErrForbidden
	}
	scope, err := s.newHistoryReadContextTx(ctx, tx, id, true)
	if errors.Is(err, ErrForbidden) {
		return deny()
	}
	if err != nil {
		return CrossConversationSearchPage{}, err
	}
	candidates, err := listCrossSearchCandidatesTx(ctx, tx, id, binding.Kind, position)
	if err != nil {
		return CrossConversationSearchPage{}, err
	}
	type staged struct {
		candidate historyCandidate
		batch     historyReadBatch
	}
	batches := make([]staged, 0, 20)
	remaining, provisional := 500, 0
	progress := position
	more := false
	for i, c := range candidates {
		if i == 20 || remaining == 0 {
			more = true
			break
		}
		after := int64(0)
		if position.Phase == "within" && position.Conversation == c.ID {
			after = position.After
		}
		read := readDirectTextSearchBatchTx
		if c.Kind == "group" {
			read = readGroupTextSearchBatchTx
		}
		batch, err := read(ctx, tx, scope, c.ID, after, remaining)
		if errors.Is(err, ErrMessageNotAvailable) {
			progress = crossSearchPosition{c.ID, "after", 0}
			continue
		}
		if err != nil {
			return CrossConversationSearchPage{}, err
		}
		at := s.now()
		if !memberActiveAt(scope.Actor, at) {
			return deny()
		}
		visible := filterDirectHistoryBatch(scope, batch, at)
		if c.Kind == "group" {
			visible, err = filterGroupHistoryBatchTx(ctx, tx, scope, batch, at)
			if err != nil {
				return CrossConversationSearchPage{}, err
			}
		}
		remaining -= len(batch.Page.Messages)
		progress = crossSearchPosition{c.ID, "after", 0}
		if batch.Page.HasMore {
			progress = crossSearchPosition{c.ID, "within", batch.Page.NextAfterSeq}
			more = true
		}
		stopped := false
		for j, m := range visible.Messages {
			if m.MessageType == MessageTypeText && !m.Redacted && strings.Contains(strings.ToLower(m.Text), binding.Query) {
				provisional++
			}
			if provisional == limit {
				// Keep only the processed prefix; later matches are resumed, never skipped.
				batch.Page.Messages = batch.Page.Messages[:j+1]
				batch.Direct = batch.Direct[:j+1]
				progress = crossSearchPosition{c.ID, "within", m.Seq}
				more = true
				stopped = true
				break
			}
		}
		batches = append(batches, staged{c, batch})
		if stopped {
			break
		}
	}
	scope, err = refreshCrossSearchPolicyTx(ctx, tx, scope)
	if err != nil {
		return CrossConversationSearchPage{}, err
	}
	at := s.now()
	if !memberActiveAt(scope.Actor, at) {
		return deny()
	}
	page := CrossConversationSearchPage{Messages: make([]CrossConversationMatch, 0, limit), HasMore: more}
	finalFull := false
	for _, entry := range batches {
		visible := filterDirectHistoryBatch(scope, entry.batch, at)
		if entry.candidate.Kind == "group" {
			visible, err = filterGroupHistoryBatchTx(ctx, tx, scope, entry.batch, at)
			if err != nil {
				return CrossConversationSearchPage{}, err
			}
		}
		for _, m := range visible.Messages {
			if m.MessageType != MessageTypeText || m.Redacted || !strings.Contains(strings.ToLower(m.Text), binding.Query) {
				continue
			}
			page.Messages = append(page.Messages, CrossConversationMatch{entry.candidate.ID, entry.candidate.Kind, m})
			if len(page.Messages) == limit {
				progress = crossSearchPosition{entry.candidate.ID, "within", m.Seq}
				page.HasMore = true
				finalFull = true
				break
			}
		}
		if finalFull {
			break
		}
	}
	if page.HasMore {
		page.NextCursor = encodeCrossSearchCursor(binding, progress)
	}
	if err := auditCrossSearch(ctx, tx, id, "allow", "cross_conversation_search_page", at); err != nil {
		return CrossConversationSearchPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CrossConversationSearchPage{}, err
	}
	return page, nil
}
