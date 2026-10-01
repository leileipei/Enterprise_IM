package policystore

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

var ErrInvalidGroupListRequest = errors.New("invalid group list request")

type ListedGroup struct {
	ID                 string
	Name               string
	Status             string
	Role               string
	SourceMembershipID string
	LastSeq            int64
	UpdatedAt          time.Time
}

type GroupListPage struct {
	Groups     []ListedGroup
	HasMore    bool
	NextCursor string
}

func auditGroupList(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_list','conversation',NULL,$4,$5,$6)`,
		id.TenantID, id.UserID, id.ActingMembershipID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

// ListGroups returns current group memberships for the caller's user. A valid
// acting membership is required to read, while each group's source membership
// is returned so the client can select it before attempting a send.
func (s Service) ListGroups(ctx context.Context, id access.TrustedIdentity,
	cursor string, limit int) (GroupListPage, error) {
	for attempt := 0; ; attempt++ {
		page, err := s.listGroupsOnce(ctx, id, cursor, limit)
		var databaseError *pgconn.PgError
		if attempt >= 2 || !errors.As(err, &databaseError) || databaseError.Code != "40001" || ctx.Err() != nil {
			return page, err
		}
	}
}

func (s Service) listGroupsOnce(ctx context.Context, id access.TrustedIdentity,
	cursor string, limit int) (GroupListPage, error) {
	if limit < 1 || limit > 50 {
		return GroupListPage{}, ErrInvalidGroupListRequest
	}
	before, beforeID, err := decodeConversationCursor(cursor)
	if err != nil {
		return GroupListPage{}, ErrInvalidGroupListRequest
	}
	if s.DB == nil || !directoryUUIDPattern.MatchString(id.TenantID) ||
		!directoryUUIDPattern.MatchString(id.UserID) ||
		!directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return GroupListPage{}, ErrForbidden
	}
	id.TenantID, id.UserID, id.ActingMembershipID = strings.ToLower(id.TenantID),
		strings.ToLower(id.UserID), strings.ToLower(id.ActingMembershipID)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupListPage{}, err
	}
	defer tx.Rollback(ctx)
	// A stable statement snapshot alone is insufficient when FOR SHARE waits
	// behind a writer: READ COMMITTED may return a newer updated_at in its old
	// sorted position. Retry if the locked row changed since this snapshot.
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return GroupListPage{}, err
	}
	at := s.now()
	actor, found, err := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if err != nil {
		return GroupListPage{}, err
	}
	if !found || !memberActiveAt(actor, at) {
		if err := auditGroupList(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return GroupListPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupListPage{}, err
		}
		return GroupListPage{}, ErrForbidden
	}
	query := `SELECT c.id::text,c.group_name,c.status,c.last_seq,c.updated_at,
 i.role,i.source_membership_id::text
 FROM conversation_membership_intervals i
 JOIN conversations c ON c.tenant_id=i.tenant_id AND c.id=i.conversation_id
 WHERE i.tenant_id=$1 AND i.user_id=$2 AND i.status='active'
   AND c.kind='group' AND c.status IN ('active','policy_blocked')`
	args := []any{id.TenantID, id.UserID}
	if cursor != "" {
		query += ` AND (c.updated_at,c.id)<($3::timestamptz,$4::uuid)`
		args = append(args, before, beforeID)
	}
	query += ` ORDER BY c.updated_at DESC,c.id DESC LIMIT $` +
		strconv.Itoa(len(args)+1) + ` FOR SHARE OF c,i`
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return GroupListPage{}, err
	}
	groups := make([]ListedGroup, 0, limit+1)
	for rows.Next() {
		var group ListedGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.Status, &group.LastSeq,
			&group.UpdatedAt, &group.Role, &group.SourceMembershipID); err != nil {
			rows.Close()
			return GroupListPage{}, err
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GroupListPage{}, err
	}
	page := GroupListPage{Groups: make([]ListedGroup, 0, min(len(groups), limit))}
	page.HasMore = len(groups) > limit
	if page.HasMore {
		groups = groups[:limit]
	}
	page.Groups = append(page.Groups, groups...)
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		if err := auditGroupList(ctx, tx, id, "deny", "invalid_identity", at); err != nil {
			return GroupListPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupListPage{}, err
		}
		return GroupListPage{}, ErrForbidden
	}
	if page.HasMore {
		last := page.Groups[len(page.Groups)-1]
		page.NextCursor = encodeConversationCursor(last.UpdatedAt, last.ID)
	}
	if err := auditGroupList(ctx, tx, id, "allow", "page", at); err != nil {
		return GroupListPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GroupListPage{}, err
	}
	return page, nil
}
