package policystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
)

var ErrGroupRecheckPermissionDenied = errors.New("group policy recheck requires owner or administrator")

type GroupPolicyRecheck struct {
	Status        string
	PolicyVersion int64
}

func auditGroupPolicyRecheck(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity,
	groupID, outcome, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
 (tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at)
 VALUES ($1,$2,$3,'group_policy_recheck','conversation',$4,$5,$6,$7)`,
		id.TenantID, id.UserID, id.ActingMembershipID, groupID, outcome, reason, at)
	if err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

func (s Service) RecheckGroupPolicy(ctx context.Context, id access.TrustedIdentity,
	groupID string) (GroupPolicyRecheck, error) {
	for attempt := 0; attempt < 3; attempt++ {
		result, err := s.recheckGroupPolicyOnce(ctx, id, groupID)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "40P01" ||
			attempt == 2 || ctx.Err() != nil {
			return result, err
		}
	}
	return GroupPolicyRecheck{}, ErrPolicyUnavailable
}

func (s Service) recheckGroupPolicyOnce(ctx context.Context, id access.TrustedIdentity,
	groupID string) (GroupPolicyRecheck, error) {
	if s.DB == nil {
		return GroupPolicyRecheck{}, ErrForbidden
	}
	id, groupID, err := normalizeGroupIdentity(id, groupID)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := s.activeGroupActor(ctx, tx, id)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM conversations
 WHERE tenant_id=$1 AND id=$2 AND kind='group' FOR UPDATE`, id.TenantID, groupID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status == "ended" {
		return GroupPolicyRecheck{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM conversation_membership_intervals
 WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3 AND status='active' FOR SHARE`,
		id.TenantID, groupID, id.UserID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupPolicyRecheck{}, ErrGroupNotAvailable
	}
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	at := s.now()
	if !memberActiveAt(actor, at) {
		return GroupPolicyRecheck{}, ErrForbidden
	}
	if role != "owner" && role != "admin" {
		if err := auditGroupPolicyRecheck(ctx, tx, id, groupID, "deny", "group_permission_denied", at); err != nil {
			return GroupPolicyRecheck{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupPolicyRecheck{}, err
		}
		return GroupPolicyRecheck{}, ErrGroupRecheckPermissionDenied
	}
	refs, err := loadInviteMembers(ctx, tx, id.TenantID, groupID)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	if len(refs) == 0 {
		return GroupPolicyRecheck{}, ErrGroupNotAvailable
	}
	version, err := currentVersion(ctx, tx, id.TenantID)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	rules, err := loadRules(ctx, tx, id.TenantID, version)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	loaded, err := loadGroupSendMemberships(ctx, tx, id.TenantID, refs)
	if err != nil {
		return GroupPolicyRecheck{}, err
	}
	members := make([]groupMember, 0, len(refs))
	missing := false
	for _, ref := range refs {
		member, found := loaded[ref.membershipID]
		if !found {
			missing = true
			continue
		}
		members = append(members, groupMember{userID: ref.userID, membership: member})
	}
	check := func(at time.Time) (string, *groupSendFailure) {
		if missing {
			return "member_unavailable", nil
		}
		for _, member := range members {
			if !memberActiveAt(member.membership, at) {
				return "member_unavailable", nil
			}
		}
		if failure := evaluateGroupSendPairs(members, at, version, rules); failure != nil {
			return string(failure.decision.Reason), failure
		}
		return "", nil
	}
	finishBlocked := func(at time.Time, reason string, failure *groupSendFailure) (GroupPolicyRecheck, error) {
		if failure != nil {
			if err := auditGroupSendDenial(ctx, tx, id, failure, at); err != nil {
				return GroupPolicyRecheck{}, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE conversations
 SET status='policy_blocked',last_policy_version=$3,updated_at=$4
 WHERE tenant_id=$1 AND id=$2`, id.TenantID, groupID, version, at); err != nil {
			return GroupPolicyRecheck{}, err
		}
		if err := auditGroupPolicyRecheck(ctx, tx, id, groupID, "deny", reason, at); err != nil {
			return GroupPolicyRecheck{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return GroupPolicyRecheck{}, err
		}
		return GroupPolicyRecheck{}, ErrGroupPolicyBlocked
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
	}
	if !memberActiveAt(actor, at) {
		return GroupPolicyRecheck{}, ErrForbidden
	}
	if reason, failure := check(at); reason != "" {
		return finishBlocked(at, reason, failure)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT group_recheck_allow"); err != nil {
		return GroupPolicyRecheck{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET status='active',
 last_policy_version=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2`,
		id.TenantID, groupID, version, at); err != nil {
		return GroupPolicyRecheck{}, err
	}
	if err := auditGroupPolicyRecheck(ctx, tx, id, groupID, "allow", "policy_compliant", at); err != nil {
		return GroupPolicyRecheck{}, err
	}
	if fresh := s.now(); fresh.After(at) {
		at = fresh
		if !memberActiveAt(actor, at) {
			return GroupPolicyRecheck{}, ErrForbidden
		}
		if reason, failure := check(at); reason != "" {
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT group_recheck_allow"); err != nil {
				return GroupPolicyRecheck{}, err
			}
			return finishBlocked(at, reason, failure)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return GroupPolicyRecheck{}, err
	}
	return GroupPolicyRecheck{Status: "active", PolicyVersion: version}, nil
}
