package policystore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policy"
)

// Immutable in-memory proof: no transaction, database, object client or I/O.
type fileVisibilityFacts struct {
	metadata            files.Metadata
	messageID           string
	seq                 int64
	actor, uploader     policy.Membership
	scope               historyReadContext
	batch               historyReadBatch
	groupDenials, rules []policy.Rule
	version             int64
	accepted            time.Time
	days                int64
	bodyRetention       time.Duration
}

func loadFileVisibilityFactsTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, fileID string) (fileVisibilityFacts, error) {
	fail := func(e error) (fileVisibilityFacts, error) { return fileVisibilityFacts{}, e }
	var e error
	id, e = fileIdentity(id)
	if e != nil {
		return fail(filedownload.ErrInvalidIdentity)
	}
	actor, found, e := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if e != nil {
		return fail(e)
	}
	fresh, e := fileClock(ctx, tx)
	if e != nil {
		return fail(e)
	}
	if !found || !memberActiveAt(actor, fresh) {
		return fail(filedownload.ErrInvalidIdentity)
	}
	if !directoryUUIDPattern.MatchString(fileID) {
		return fail(filedownload.ErrNotFound)
	}
	fileID = strings.ToLower(fileID)
	m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2", id.TenantID, fileID))
	if errors.Is(e, pgx.ErrNoRows) {
		return fail(filedownload.ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	// Lock all identity parents before the conversation. NOWAIT avoids a reversed
	// lock acquisition in existing callers; bounded transaction retry is external.
	rows, e := tx.Query(ctx, `SELECT u.id FROM user_organizations m
 JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id
 JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id
 JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id
 JOIN tenants t ON t.id=m.tenant_id
 WHERE m.tenant_id=$1 AND m.id=ANY($2::uuid[]) ORDER BY m.id FOR SHARE OF t,u,m,o,l NOWAIT`, id.TenantID, []string{id.ActingMembershipID, m.UploaderMembershipID})
	if e != nil {
		return fail(e)
	}
	for rows.Next() {
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return fail(e)
	}
	refs := []inviteMemberRef{{userID: id.UserID, membershipID: id.ActingMembershipID}, {userID: m.UploaderUserID, membershipID: m.UploaderMembershipID}}
	loaded, e := loadGroupSendMemberships(ctx, tx, id.TenantID, refs)
	if e != nil {
		return fail(e)
	}
	var kind, status string
	e = tx.QueryRow(ctx, "SELECT kind,status FROM conversations WHERE tenant_id=$1 AND id=$2 FOR UPDATE NOWAIT", id.TenantID, m.ConversationID).Scan(&kind, &status)
	if errors.Is(e, pgx.ErrNoRows) {
		return fail(filedownload.ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	var pointer int64
	e = tx.QueryRow(ctx, "SELECT current_version FROM policy_current WHERE tenant_id=$1 FOR SHARE", id.TenantID).Scan(&pointer)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return fail(e)
	}
	version, e := currentVersion(ctx, tx, id.TenantID)
	if e != nil {
		return fail(e)
	}
	rules, e := loadRules(ctx, tx, id.TenantID, version)
	if e != nil {
		return fail(e)
	}
	var days int64
	if e = tx.QueryRow(ctx, "SELECT file_retention_days FROM tenant_file_retention_policy WHERE tenant_id=$1 FOR SHARE", id.TenantID).Scan(&days); e != nil {
		return fail(e)
	}
	bodyRetention, e := messageBodyRetentionForTenant(ctx, tx, id.TenantID)
	if e != nil {
		return fail(e)
	}
	m, e = scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 FOR SHARE", id.TenantID, fileID))
	if errors.Is(e, pgx.ErrNoRows) {
		return fail(filedownload.ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	if m.State != files.StateReady || files.ValidateMetadata(m) != nil {
		return fail(filedownload.ErrNotFound)
	}
	var mid string
	var seq int64
	var fingerprint []byte
	var accepted time.Time
	e = tx.QueryRow(ctx, `SELECT m.id::text,m.seq,a.sealed_sha256,m.accepted_at
 FROM message_attachments a JOIN messages m ON m.tenant_id=a.tenant_id AND m.id=a.message_id
 AND m.conversation_id=a.conversation_id AND m.sender_user_id=a.sender_user_id AND m.sender_membership_id=a.sender_membership_id
 WHERE a.tenant_id=$1 AND a.file_id=$2 AND a.conversation_id=$3
 AND a.sender_user_id=$4 AND a.sender_membership_id=$5 AND m.message_type='file'
 AND m.text_body IS NOT NULL AND m.body_cleared_at IS NULL FOR SHARE OF m,a`, id.TenantID, m.ID, m.ConversationID, m.UploaderUserID, m.UploaderMembershipID).Scan(&mid, &seq, &fingerprint, &accepted)
	if errors.Is(e, pgx.ErrNoRows) {
		return fail(filedownload.ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	if seq < 1 || !bytes.Equal(fingerprint, m.SHA256) {
		return fail(filedownload.ErrNotFound)
	}
	scope := historyReadContext{Identity: id, Actor: actor, Rules: rules, Retention: bodyRetention}
	var batch historyReadBatch
	if kind == "direct" {
		batch, e = readDirectHistoryBatchTx(ctx, tx, scope, m.ConversationID, seq-1, 1)
	} else if kind == "group" {
		batch, e = readGroupHistoryBatchTx(ctx, tx, scope, m.ConversationID, seq-1, 1)
	} else {
		return fail(filedownload.ErrNotFound)
	}
	if errors.Is(e, ErrMessageNotAvailable) {
		return fail(filedownload.ErrNotFound)
	}
	if e != nil {
		return fail(e)
	}
	actor, found = loaded[id.ActingMembershipID]
	if !found {
		return fail(filedownload.ErrInvalidIdentity)
	}
	scope.Actor = actor
	page := batch.Page
	if len(page.Messages) != 1 || page.Messages[0].Redacted || page.Messages[0].MessageID != mid || page.Messages[0].Attachment == nil || !page.Messages[0].Attachment.Available || page.Messages[0].Attachment.FileID != m.ID || len(batch.Direct) != 1 || batch.Direct[0].reader.ID != id.ActingMembershipID {
		return fail(filedownload.ErrNotFound)
	}
	if status != "active" {
		return fail(filedownload.ErrNotFound)
	}
	if kind == "direct" {
		d, e := loadDirectMessageContext(ctx, tx, id.TenantID, m.ConversationID, false)
		if e != nil {
			return fail(e)
		}
		for _, r := range refs {
			selected, _, ok := d.selectedMembership(r.userID)
			if !ok || selected != r.membershipID {
				return fail(filedownload.ErrNotFound)
			}
		}
	} else {
		for _, r := range refs {
			var present bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_membership_intervals WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND source_membership_id=$4 AND status='active')`, id.TenantID, m.ConversationID, r.userID, r.membershipID).Scan(&present); e != nil {
				return fail(e)
			}
			if !present {
				return fail(filedownload.ErrNotFound)
			}
		}
	}
	var groupDenials []policy.Rule
	if kind == "group" {
		groupDenials, e = groupHistoryMatchingDenials(ctx, tx, id, m.ConversationID, actor, batch.ReaderIntervals, rules)
		if e != nil {
			return fail(e)
		}
	}
	uploader, found := loaded[m.UploaderMembershipID]
	if !found {
		return fail(filedownload.ErrNotFound)
	}
	return fileVisibilityFacts{metadata: m, messageID: mid, seq: seq, actor: actor, uploader: uploader, scope: scope, batch: batch, groupDenials: groupDenials, rules: rules, version: version, accepted: accepted, days: days, bodyRetention: bodyRetention}, nil
}

// Every temporal decision is made against the caller's final database time.
func evaluateFileVisibility(f fileVisibilityFacts, fresh time.Time) (files.Metadata, string, int64, error) {
	fail := func(e error) (files.Metadata, string, int64, error) { return files.Metadata{}, "", 0, e }
	m, mid, seq := f.metadata, f.messageID, f.seq
	actor, uploader := f.actor, f.uploader
	scope, batch, groupDenials := f.scope, f.batch, f.groupDenials
	rules, version, accepted, days, bodyRetention := f.rules, f.version, f.accepted, f.days, f.bodyRetention
	if !memberActiveAt(actor, fresh) {
		return fail(filedownload.ErrInvalidIdentity)
	}
	if p := filterDirectHistoryBatch(scope, batch, fresh); len(p.Messages) != 1 || p.Messages[0].Redacted || groupHistoryDenialsActive(groupDenials, fresh) {
		return fail(filedownload.ErrNotFound)
	}
	if !memberActiveAt(uploader, fresh) {
		return fail(filedownload.ErrNotFound)
	}
	for _, action := range []policy.Action{policy.ActionSendMessage, policy.ActionFileDownload} {
		if downloadPairHardDenied(actor, uploader, action, rules, fresh) {
			return fail(filedownload.ErrNotFound)
		}
		decision := policy.Evaluate(policy.Input{Action: action, Actor: actor, Target: uploader, At: fresh, ScopeAllowed: true, ResourceActive: true, PolicyVersion: version, Rules: rules})
		if !decision.Allowed {
			return fail(filedownload.ErrNotFound)
		}
	}
	expires, e := files.FileExpiresAt(accepted, days)
	if e != nil || !fresh.Before(expires) || !fresh.Before(accepted.Add(bodyRetention)) {
		return fail(filedownload.ErrNotFound)
	}
	return m, mid, seq, nil
}
