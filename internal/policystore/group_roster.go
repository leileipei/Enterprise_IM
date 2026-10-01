package policystore

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

var (
	ErrInvalidGroupRosterRequest   = errors.New("invalid group roster request")
	ErrGroupRosterPermissionDenied = errors.New("group roster permission denied")
)

type GroupRosterMember struct {
	IntervalID       string
	DisplayName      string
	Role             string
	OrganizationName string
	JoinedAt         time.Time
}

type GroupRosterPage struct {
	Members    []GroupRosterMember
	HasMore    bool
	NextCursor string
}

func auditGroupRoster(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, groupID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_roster_list','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, groupID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

// ListGroupMembers returns a bounded page of current group intervals to an
// active owner or administrator. It never exposes historical intervals.
func (s Service) ListGroupMembers(ctx context.Context, id access.TrustedIdentity, groupID, cursor string, limit int) (GroupRosterPage, error) {
	if limit < 1 || limit > 50 {
		return GroupRosterPage{}, ErrInvalidGroupRosterRequest
	}
	before, beforeID, err := decodeConversationCursor(cursor)
	if err != nil {
		return GroupRosterPage{}, ErrInvalidGroupRosterRequest
	}
	if s.DB == nil {
		return GroupRosterPage{}, ErrForbidden
	}
	id, groupID, err = normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupRosterPage{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupRosterPage{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := s.activeGroupActor(ctx, tx, id)
	if err != nil {
		return GroupRosterPage{}, err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM conversations WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR SHARE`,
		id.TenantID, groupID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupRosterPage{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupRosterPage{}, err
	}
	if status != "active" && status != "policy_blocked" {
		return GroupRosterPage{}, ErrGroupNotAvailable
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND status='active' FOR SHARE`,
		id.TenantID, groupID, id.UserID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupRosterPage{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupRosterPage{}, err
	}
	if role != "owner" && role != "admin" {
		if err := auditGroupRoster(ctx, tx, id, groupID, "deny", "group_permission_denied", s.now()); err != nil {
			return GroupRosterPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupRosterPage{}, err
		}
		return GroupRosterPage{}, ErrGroupRosterPermissionDenied
	}
	query := `SELECT i.id::text,u.display_name,i.role,o.name,i.joined_at
 FROM conversation_membership_intervals i
 JOIN users u ON u.tenant_id=i.tenant_id AND u.id=i.user_id
 JOIN organizations o ON o.tenant_id=i.tenant_id AND o.id=i.source_organization_id
 WHERE i.tenant_id=$1 AND i.conversation_id=$2 AND i.status='active'`
	args := []any{id.TenantID, groupID}
	if cursor != "" {
		query += ` AND (i.joined_at,i.id)<($3::timestamptz,$4::uuid)`
		args = append(args, before, beforeID)
	}
	query += ` ORDER BY i.joined_at DESC,i.id DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return GroupRosterPage{}, err
	}
	members := make([]GroupRosterMember, 0, limit+1)
	for rows.Next() {
		var member GroupRosterMember
		if err := rows.Scan(&member.IntervalID, &member.DisplayName, &member.Role, &member.OrganizationName, &member.JoinedAt); err != nil {
			rows.Close()
			return GroupRosterPage{}, err
		}
		members = append(members, member)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GroupRosterPage{}, err
	}
	if !memberActiveAt(actor, s.now()) {
		return GroupRosterPage{}, ErrForbidden
	}
	page := GroupRosterPage{Members: make([]GroupRosterMember, 0, min(len(members), limit)), HasMore: len(members) > limit}
	if page.HasMore {
		members = members[:limit]
	}
	page.Members = append(page.Members, members...)
	if page.HasMore {
		last := page.Members[len(page.Members)-1]
		page.NextCursor = encodeConversationCursor(last.JoinedAt, last.IntervalID)
	}
	if err := auditGroupRoster(ctx, tx, id, groupID, "allow", "page", s.now()); err != nil {
		return GroupRosterPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GroupRosterPage{}, err
	}
	return page, nil
}
