package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"slices"
	"strings"
	"time"
)

var errFileContextRetry = errors.New("file authorization context changed during lock acquisition")

func fileIdentity(id access.TrustedIdentity) (access.TrustedIdentity, error) {
	if !directoryUUIDPattern.MatchString(id.TenantID) || !directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return id, files.ErrInvalidIdentity
	}
	id.TenantID = strings.ToLower(id.TenantID)
	id.UserID = strings.ToLower(id.UserID)
	id.ActingMembershipID = strings.ToLower(id.ActingMembershipID)
	return id, nil
}
func fileClock(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var at time.Time
	e := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at)
	return at, e
}
func fileRefs(ctx context.Context, tx pgx.Tx, tenant, convo, kind string, lock bool) ([]inviteMemberRef, error) {
	if kind == "direct" {
		d, e := loadDirectMessageContext(ctx, tx, tenant, convo, false)
		if e != nil {
			return nil, e
		}
		return []inviteMemberRef{{userID: d.lowUserID, membershipID: d.lowMember}, {userID: d.highUserID, membershipID: d.highMember}}, nil
	}
	if kind != "group" {
		return nil, files.ErrFileNotFound
	}
	q := `SELECT user_id::text,source_membership_id::text FROM conversation_membership_intervals WHERE tenant_id=$1 AND conversation_id=$2 AND status='active' ORDER BY user_id`
	if lock {
		q += " FOR SHARE"
	}
	rows, e := tx.Query(ctx, q, tenant, convo)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var refs []inviteMemberRef
	for rows.Next() {
		var ref inviteMemberRef
		if e = rows.Scan(&ref.userID, &ref.membershipID); e != nil {
			return nil, e
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// Authorization uses the existing message membership and full pair policy evaluators.
// No message sequence, message rate, ACK, or Outbox operation runs in this path.
func authorizeFileTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID string, requireSend bool) (time.Time, error) {
	at, e := fileClock(ctx, tx)
	if e != nil {
		return at, e
	}
	actor, found, e := loadMembershipSnapshot(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if e != nil {
		return at, e
	}
	if !found || !memberActiveAt(actor, at) {
		return at, files.ErrInvalidIdentity
	}
	var kind, status string
	e = tx.QueryRow(ctx, "SELECT kind,status FROM conversations WHERE tenant_id=$1 AND id=$2", id.TenantID, conversationID).Scan(&kind, &status)
	if errors.Is(e, pgx.ErrNoRows) {
		return at, files.ErrFileNotFound
	}
	if e != nil {
		return at, e
	}
	refs, e := fileRefs(ctx, tx, id.TenantID, conversationID, kind, false)
	if e != nil {
		return at, e
	}
	participant := false
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.membershipID)
		if r.userID == id.UserID && r.membershipID == id.ActingMembershipID {
			participant = true
		}
	}
	if !participant {
		return at, files.ErrFileNotFound
	}
	slices.Sort(ids)
	rows, e := tx.Query(ctx, "SELECT id FROM user_organizations WHERE tenant_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR SHARE NOWAIT", id.TenantID, ids)
	if e != nil {
		return at, e
	}
	for rows.Next() {
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return at, e
	}
	loaded, e := loadGroupSendMemberships(ctx, tx, id.TenantID, refs)
	if e != nil {
		return at, e
	}
	e = tx.QueryRow(ctx, "SELECT kind,status FROM conversations WHERE tenant_id=$1 AND id=$2 FOR UPDATE", id.TenantID, conversationID).Scan(&kind, &status)
	if e != nil {
		return at, e
	}
	current, e := fileRefs(ctx, tx, id.TenantID, conversationID, kind, true)
	if e != nil {
		return at, e
	}
	if !slices.Equal(refs, current) {
		return at, errFileContextRetry
	}
	at, e = fileClock(ctx, tx)
	if e != nil {
		return at, e
	}
	actor, found = loaded[id.ActingMembershipID]
	if !found || !memberActiveAt(actor, at) {
		return at, files.ErrInvalidIdentity
	}
	if status != "active" && (!requireSend && status != "policy_blocked" || requireSend) {
		return at, files.ErrFileNotFound
	}
	if !requireSend {
		return at, nil
	}
	version, e := currentVersion(ctx, tx, id.TenantID)
	if e != nil {
		return at, e
	}
	rules, e := loadRules(ctx, tx, id.TenantID, version)
	if e != nil {
		return at, e
	}
	members := make([]groupMember, 0, len(refs))
	for _, r := range refs {
		m, found := loaded[r.membershipID]
		if !found || !memberActiveAt(m, at) {
			return at, files.ErrFileNotFound
		}
		members = append(members, groupMember{userID: r.userID, membership: m})
	}
	if evaluateGroupSendPairs(members, at, version, rules) != nil {
		return at, files.ErrFileNotFound
	}
	return at, nil
}
func fileTransaction[T any](ctx context.Context, s Service, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	if s.DB == nil {
		return zero, files.ErrDependencyUnavailable
	}
	for attempt := 0; attempt < 3; attempt++ {
		tx, e := s.DB.Begin(ctx)
		if e != nil {
			return zero, e
		}
		v, e := func() (T, error) {
			defer tx.Rollback(ctx)
			if _, e := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); e != nil {
				return zero, e
			}
			v, e := fn(tx)
			if e != nil {
				return zero, e
			}
			if e = tx.Commit(ctx); e != nil {
				return zero, e
			}
			return v, nil
		}()
		var pe *pgconn.PgError
		retry := errors.Is(e, errFileContextRetry) || (errors.As(e, &pe) && (pe.Code == "40P01" || pe.Code == "55P03"))
		if !retry || attempt == 2 || ctx.Err() != nil {
			return v, e
		}
	}
	return zero, files.ErrDependencyUnavailable
}
