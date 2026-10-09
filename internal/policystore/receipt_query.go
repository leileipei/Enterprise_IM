package policystore

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"strings"
	"time"
)

type receiptQueryContext struct {
	tx           pgx.Tx
	id           access.TrustedIdentity
	device       DeviceCredential
	cid, kind    string
	scope        historyReadContext
	clientPolicy ClientPolicy
	expiry       time.Time
}

// Queries use one database snapshot for facts, revision and authorization.
// A concurrent row change detected by PostgreSQL is returned for safe retry.
func (s Service) beginReceiptQuery(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string) (receiptQueryContext, error) {
	id, device, cid, e := normalizeReceiptRequest(id, device, cid)
	if e != nil {
		return receiptQueryContext{}, e
	}
	if s.DB == nil {
		return receiptQueryContext{}, ErrForbidden
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return receiptQueryContext{}, e
	}
	q := receiptQueryContext{tx: tx, id: id, device: device, cid: cid}
	fail := func(e error) (receiptQueryContext, error) { tx.Rollback(ctx); return receiptQueryContext{}, e }
	if _, e = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); e != nil {
		return fail(e)
	}
	q.scope, e = s.newHistoryReadContextTx(ctx, tx, id, true)
	if e != nil {
		return fail(e)
	}
	q.clientPolicy, e = clientPolicyTx(ctx, tx, id.TenantID, false)
	if e != nil {
		return fail(e)
	}
	q.kind, e = receiptConversationKindTx(ctx, tx, id, cid)
	if e != nil {
		return fail(e)
	}
	// Establish participation without inspecting message bodies or consuming the scan budget.
	var participant bool
	if q.kind == "direct" {
		e = tx.QueryRow(ctx, `SELECT direct_user_low_id=$3 OR direct_user_high_id=$3 FROM conversations WHERE tenant_id=$1 AND id=$2`, id.TenantID, cid, id.UserID).Scan(&participant)
	} else {
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_membership_intervals WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3)`, id.TenantID, cid, id.UserID).Scan(&participant)
	}
	if e != nil {
		return fail(e)
	}
	if !participant {
		return fail(ErrReceiptNotAvailable)
	}
	// Reject invalid credentials before any costly receipt or unread scan.
	if e = s.lockReceiptQueryDevice(ctx, &q); e != nil {
		return fail(e)
	}
	return q, nil
}
func (s Service) lockReceiptQueryDevice(ctx context.Context, q *receiptQueryContext) error {
	expiry, e := lockDeviceCredentialTx(ctx, q.tx, DeviceOwner{q.id.TenantID, q.id.UserID}, q.device, s.now(), false)
	q.expiry = expiry
	return e
}
func (s Service) finalReceiptQuery(ctx context.Context, q receiptQueryContext) error {
	if _, e := s.activeGroupActor(ctx, q.tx, q.id); e != nil {
		return e
	}
	if !s.now().Before(q.expiry) {
		return ErrInvalidDevice
	}
	return nil
}
func senderReceiptBatchTx(ctx context.Context, q receiptQueryContext, seqs []int64) (historyReadBatch, error) {
	batch, e := receiptHistoryBatchTx(ctx, q.tx, q.scope, q.cid, q.kind, 0, len(seqs), seqs)
	if e != nil {
		return historyReadBatch{}, e
	}
	if len(batch.Page.Messages) != len(seqs) {
		return historyReadBatch{}, ErrReceiptNotAvailable
	}
	return batch, nil
}
func senderReceiptPageTx(ctx context.Context, q receiptQueryContext, batch historyReadBatch, now func() time.Time) (MessagePage, error) {
	page, e := receiptFinalPageTx(ctx, q.tx, q.scope, batch, q.kind, now)
	if e != nil {
		return MessagePage{}, e
	}
	for _, m := range page.Messages {
		if m.Redacted || m.SenderUserID != q.id.UserID {
			return MessagePage{}, ErrReceiptNotAvailable
		}
	}
	return page, nil
}

// Receipt roster privileges are bound to the current interval's source acting
// membership. Knowing a group or a continuation cursor confers no grant.
func receiptGroupAdminTx(ctx context.Context, q receiptQueryContext) (bool, error) {
	var role, source string
	var status string
	e := q.tx.QueryRow(ctx, `SELECT i.role,i.source_membership_id::text,c.status FROM conversation_membership_intervals i JOIN conversations c ON c.tenant_id=i.tenant_id AND c.id=i.conversation_id WHERE i.tenant_id=$1 AND i.conversation_id=$2 AND i.user_id=$3 AND i.status='active' FOR SHARE OF i,c`, q.id.TenantID, q.cid, q.id.UserID).Scan(&role, &source, &status)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return source == q.id.ActingMembershipID && (role == "owner" || role == "admin") && (status == "active" || status == "policy_blocked"), nil
}

type receiptCandidate struct {
	seq             int64
	userID, name    string
	delivered, read *time.Time
}
type receiptDirectoryMember struct {
	userID string
	member policy.Membership
}

func receiptCandidatesTx(ctx context.Context, q receiptQueryContext, seqs []int64) ([]receiptCandidate, []receiptDirectoryMember, error) {
	query := `SELECT m.seq,u.id::text,u.display_name,r.delivered_at,r.read_at FROM messages m JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.recipient_user_id LEFT JOIN message_user_receipts r ON r.tenant_id=m.tenant_id AND r.conversation_id=m.conversation_id AND r.seq=m.seq AND r.user_id=u.id WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.seq=ANY($3::bigint[]) ORDER BY m.seq,u.id`
	if q.kind == "group" {
		query = `SELECT m.seq,u.id::text,u.display_name,r.delivered_at,r.read_at FROM messages m JOIN users u ON u.tenant_id=m.tenant_id AND u.id<>m.sender_user_id LEFT JOIN message_user_receipts r ON r.tenant_id=m.tenant_id AND r.conversation_id=m.conversation_id AND r.seq=m.seq AND r.user_id=u.id WHERE m.tenant_id=$1 AND m.conversation_id=$2 AND m.seq=ANY($3::bigint[]) AND EXISTS(SELECT 1 FROM conversation_membership_intervals i WHERE i.tenant_id=m.tenant_id AND i.conversation_id=m.conversation_id AND i.user_id=u.id AND i.join_seq<=m.seq AND (i.leave_seq IS NULL OR m.seq<=i.leave_seq)) ORDER BY m.seq,u.id`
	}
	rows, e := q.tx.Query(ctx, query, q.id.TenantID, q.cid, seqs)
	if e != nil {
		return nil, nil, e
	}
	candidates := []receiptCandidate{}
	users := map[string]bool{}
	for rows.Next() {
		var r receiptCandidate
		if e = rows.Scan(&r.seq, &r.userID, &r.name, &r.delivered, &r.read); e != nil {
			rows.Close()
			return nil, nil, e
		}
		candidates = append(candidates, r)
		users[r.userID] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, nil, e
	}
	if q.kind == "direct" || len(users) == 0 {
		return candidates, nil, nil
	}
	userIDs := make([]string, 0, len(users))
	for u := range users {
		userIDs = append(userIDs, u)
	}
	rows, e = q.tx.Query(ctx, `SELECT m.user_id::text,m.id::text,m.tenant_id::text,m.organization_id::text,o.legal_entity_id::text,u.status,CASE WHEN t.status='active' AND o.status='active' AND l.status='active' THEN m.status ELSE 'suspended' END,m.effective_from,m.effective_to FROM user_organizations m JOIN users u ON u.tenant_id=m.tenant_id AND u.id=m.user_id JOIN organizations o ON o.tenant_id=m.tenant_id AND o.id=m.organization_id JOIN legal_entities l ON l.tenant_id=o.tenant_id AND l.id=o.legal_entity_id JOIN tenants t ON t.id=m.tenant_id WHERE m.tenant_id=$1 AND m.user_id=ANY($2::uuid[]) ORDER BY m.id FOR SHARE OF t,m,u,o,l`, q.id.TenantID, userIDs)
	if e != nil {
		return nil, nil, e
	}
	members := []receiptDirectoryMember{}
	for rows.Next() {
		var r receiptDirectoryMember
		var end *time.Time
		m := &r.member
		if e = rows.Scan(&r.userID, &m.ID, &m.TenantID, &m.OrganizationID, &m.LegalEntityID, &m.AccountStatus, &m.Status, &m.EffectiveFrom, &end); e != nil {
			rows.Close()
			return nil, nil, e
		}
		if end != nil {
			m.EffectiveTo = *end
		}
		members = append(members, r)
	}
	e = rows.Err()
	rows.Close()
	return candidates, members, e
}
func visibleReceiptCandidates(q receiptQueryContext, candidates []receiptCandidate, members []receiptDirectoryMember, at time.Time) []receiptCandidate {
	if q.kind == "direct" {
		return candidates
	}
	visible := map[string]bool{}
	for _, m := range members {
		if policy.Evaluate(policy.Input{Action: policy.ActionDirectoryView, Actor: q.scope.Actor, Target: m.member, At: at, ScopeAllowed: true, ResourceActive: true, Rules: q.scope.Rules}).Allowed {
			visible[m.userID] = true
		}
	}
	out := []receiptCandidate{}
	for _, c := range candidates {
		if visible[c.userID] {
			out = append(out, c)
		}
	}
	return out
}
func (s Service) QueryReceipts(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, seqs []int64) ([]ReceiptSummary, error) {
	if !validReceiptSeqs(seqs, 100) {
		return nil, ErrInvalidReceiptRequest
	}
	q, e := s.beginReceiptQuery(ctx, id, device, cid)
	if e != nil {
		return nil, e
	}
	defer q.tx.Rollback(ctx)
	batch, e := senderReceiptBatchTx(ctx, q, seqs)
	if e != nil {
		return nil, e
	}
	if _, e = senderReceiptPageTx(ctx, q, batch, s.now); e != nil {
		return nil, e
	}
	admin := false
	if q.kind == "group" {
		admin, e = receiptGroupAdminTx(ctx, q)
		if e != nil {
			return nil, e
		}
	}
	available := q.kind == "direct" || admin || q.clientPolicy.GroupReceiptVisibility == "sender_aggregate"
	var candidates []receiptCandidate
	var members []receiptDirectoryMember
	if available {
		candidates, members, e = receiptCandidatesTx(ctx, q, seqs)
		if e != nil {
			return nil, e
		}
	}
	if e = s.lockReceiptQueryDevice(ctx, &q); e != nil {
		return nil, e
	}
	if e = auditReceiptTx(ctx, q.tx, q.id, q.cid, "message_receipt_query", s.now()); e != nil {
		return nil, e
	}
	if e = s.finalReceiptQuery(ctx, q); e != nil {
		return nil, e
	}
	if _, e = senderReceiptPageTx(ctx, q, batch, s.now); e != nil {
		return nil, e
	}
	now := s.now()
	candidates = visibleReceiptCandidates(q, candidates, members, now)
	out := make([]ReceiptSummary, len(seqs))
	index := map[int64]int{}
	for i, seq := range seqs {
		out[i] = ReceiptSummary{Seq: seq, Status: "sent", SummaryAvailable: available}
		index[seq] = i
	}
	for _, c := range candidates {
		r := &out[index[c.seq]]
		r.VisibleRecipientCount++
		if c.delivered != nil {
			r.VisibleDeliveredCount++
		}
		if c.read != nil {
			r.VisibleReadCount++
		}
		if q.kind == "direct" {
			r.DeliveredAt = c.delivered
			r.ReadAt = c.read
		}
	}
	for i := range out {
		r := &out[i]
		if r.VisibleRecipientCount > 0 && r.VisibleReadCount == r.VisibleRecipientCount {
			r.Status = "read"
		} else if r.VisibleRecipientCount > 0 && r.VisibleDeliveredCount == r.VisibleRecipientCount {
			r.Status = "delivered"
		}
	}
	if !s.now().Before(q.expiry) {
		return nil, ErrInvalidDevice
	}
	if e = q.tx.Commit(ctx); e != nil {
		return nil, e
	}
	return out, nil
}
func decodeReceiptUserCursor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(raw)
	if e != nil || base64.RawURLEncoding.EncodeToString(b) != raw || !directoryUUIDPattern.MatchString(string(b)) || string(b) != strings.ToLower(string(b)) {
		return "", ErrInvalidReceiptRequest
	}
	return string(b), nil
}
func (s Service) ListReceiptUsers(ctx context.Context, id access.TrustedIdentity, device DeviceCredential, cid string, seq int64, cursor string, limit int) (ReceiptUserPage, error) {
	if seq < 1 || limit < 1 || limit > 50 {
		return ReceiptUserPage{}, ErrInvalidReceiptRequest
	}
	after, e := decodeReceiptUserCursor(cursor)
	if e != nil {
		return ReceiptUserPage{}, e
	}
	q, e := s.beginReceiptQuery(ctx, id, device, cid)
	if e != nil {
		return ReceiptUserPage{}, e
	}
	defer q.tx.Rollback(ctx)
	if q.kind != "group" {
		return ReceiptUserPage{}, ErrReceiptNotAvailable
	}
	batch, e := senderReceiptBatchTx(ctx, q, []int64{seq})
	if e != nil {
		return ReceiptUserPage{}, e
	}
	if _, e = senderReceiptPageTx(ctx, q, batch, s.now); e != nil {
		return ReceiptUserPage{}, e
	}
	admin, e := receiptGroupAdminTx(ctx, q)
	if e != nil {
		return ReceiptUserPage{}, e
	}
	if !admin {
		return ReceiptUserPage{}, ErrReceiptNotAvailable
	}
	candidates, members, e := receiptCandidatesTx(ctx, q, []int64{seq})
	if e != nil {
		return ReceiptUserPage{}, e
	}
	if e = s.lockReceiptQueryDevice(ctx, &q); e != nil {
		return ReceiptUserPage{}, e
	}
	if e = auditReceiptTx(ctx, q.tx, q.id, q.cid, "message_receipt_users", s.now()); e != nil {
		return ReceiptUserPage{}, e
	}
	if e = s.finalReceiptQuery(ctx, q); e != nil {
		return ReceiptUserPage{}, e
	}
	if _, e = senderReceiptPageTx(ctx, q, batch, s.now); e != nil {
		return ReceiptUserPage{}, e
	}
	now := s.now()
	page := ReceiptUserPage{Users: []ReceiptUser{}}
	for _, c := range visibleReceiptCandidates(q, candidates, members, now) {
		if c.userID <= after {
			continue
		}
		if len(page.Users) == limit {
			page.HasMore = true
			break
		}
		page.Users = append(page.Users, ReceiptUser{UserID: c.userID, DisplayName: c.name, DeliveredAt: c.delivered, ReadAt: c.read})
	}
	if page.HasMore {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.Users[len(page.Users)-1].UserID))
	}
	if !s.now().Before(q.expiry) {
		return ReceiptUserPage{}, ErrInvalidDevice
	}
	if e = q.tx.Commit(ctx); e != nil {
		return ReceiptUserPage{}, e
	}
	return page, nil
}
