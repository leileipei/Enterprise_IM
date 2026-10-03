package access

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidAuditQuery = errors.New("invalid audit query")
var auditActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type AuditEventFilter struct {
	Action, Outcome, ActorUserID string
	From, Until                  string
}

type AuditEvent struct {
	ID, ActorUserID, ActingMembershipID, Action, ResourceType, Outcome, Reason string
	ResourceID                                                                 *string
	OccurredAt                                                                 time.Time
}
type AuditEventPage struct {
	Events     []AuditEvent
	NextCursor string
}
type auditCursor struct {
	TenantID    string `json:"t"`
	Action      string `json:"a"`
	Outcome     string `json:"o"`
	ActorUserID string `json:"u,omitempty"`
	From        string `json:"f,omitempty"`
	Until       string `json:"e,omitempty"`
	At          string `json:"at"`
	ID          string `json:"id"`
}

func parseAuditCursor(value, tenantID string, filter AuditEventFilter) (*time.Time, *int64, error) {
	if value == "" {
		return nil, nil, nil
	}
	if len(value) > 1024 {
		return nil, nil, ErrInvalidAuditQuery
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 768 {
		return nil, nil, ErrInvalidAuditQuery
	}
	var c auditCursor
	if json.Unmarshal(raw, &c) != nil || c.TenantID != strings.ToLower(tenantID) || c.Action != filter.Action || c.Outcome != filter.Outcome || c.ActorUserID != filter.ActorUserID || c.From != filter.From || c.Until != filter.Until {
		return nil, nil, ErrInvalidAuditQuery
	}
	canonical, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(canonical) != value {
		return nil, nil, ErrInvalidAuditQuery
	}
	at, err := time.Parse(time.RFC3339Nano, c.At)
	if err != nil || at.IsZero() || at.UTC().Format(time.RFC3339Nano) != c.At {
		return nil, nil, ErrInvalidAuditQuery
	}
	id, err := strconv.ParseInt(c.ID, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != c.ID {
		return nil, nil, ErrInvalidAuditQuery
	}
	return &at, &id, nil
}

// ListAuditEvents reads tenant audit metadata. The page never includes its own query audit.
func (s Service) ListAuditEvents(ctx context.Context, id TrustedIdentity, filter AuditEventFilter, cursor string, limit int) (AuditEventPage, error) {
	action, outcome, actorUserID := filter.Action, filter.Outcome, strings.ToLower(filter.ActorUserID)
	if (actorUserID != "" && !legalHoldUUIDPattern.MatchString(actorUserID)) || limit < 1 || limit > 100 || (action != "" && !auditActionPattern.MatchString(action)) || (outcome != "" && outcome != "allow" && outcome != "deny") {
		return AuditEventPage{}, ErrInvalidAuditQuery
	}
	if s.DB == nil || id.TenantID == "" || id.UserID == "" || id.ActingMembershipID == "" {
		return AuditEventPage{}, ErrInvalidIdentity
	}
	from, until, err := ParseAuditTimeRange(filter.From, filter.Until)
	if err != nil {
		return AuditEventPage{}, err
	}
	filter.ActorUserID = actorUserID
	filter.From, filter.Until = auditTimeText(from), auditTimeText(until)
	beforeAt, beforeID, err := parseAuditCursor(cursor, id.TenantID, filter)
	if err != nil {
		return AuditEventPage{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return AuditEventPage{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return AuditEventPage{}, err
	}
	// Match the membership-before-tenant lock order of administrative writes.
	if err = lockLegalHoldActor(ctx, tx, id); err != nil {
		return AuditEventPage{}, err
	}
	const queryAction = "audit_events_list"
	check := func(at time.Time) error {
		grant, err := s.resolve(ctx, tx, id, at)
		if errors.Is(err, ErrInvalidIdentity) {
			return deny(ctx, tx, id, queryAction, "tenant", id.TenantID, "invalid_identity", at, ErrInvalidIdentity)
		}
		if err != nil {
			return err
		}
		if !grant.all {
			return deny(ctx, tx, id, queryAction, "tenant", id.TenantID, "not_group_admin", at, ErrNotFound)
		}
		return nil
	}
	if err = check(s.currentTime()); err != nil {
		return AuditEventPage{}, err
	}
	// Initial authorization may have waited on tenant, account, organization or grant locks.
	if err = check(s.currentTime()); err != nil {
		return AuditEventPage{}, err
	}
	var actorParam *string
	if actorUserID != "" {
		actorParam = &actorUserID
	}
	rows, err := tx.Query(ctx, `SELECT id::text,actor_user_id::text,acting_membership_id::text,action,
 resource_type,resource_id::text,outcome,reason,occurred_at
 FROM audit_events WHERE tenant_id=$1 AND ($2::text='' OR action=$2) AND ($3::text='' OR outcome=$3)
 AND ($7::uuid IS NULL OR actor_user_id=$7)
 AND ($8::timestamptz IS NULL OR occurred_at >= $8)
 AND ($9::timestamptz IS NULL OR occurred_at < $9)
 AND ($4::timestamptz IS NULL OR (occurred_at,id)<($4,$5::bigint))
 ORDER BY audit_events.occurred_at DESC,audit_events.id DESC LIMIT $6`, id.TenantID, action, outcome, beforeAt, beforeID, limit+1, actorParam, from, until)
	if err != nil {
		return AuditEventPage{}, err
	}
	events := make([]AuditEvent, 0, limit+1)
	for rows.Next() {
		var e AuditEvent
		if err = rows.Scan(&e.ID, &e.ActorUserID, &e.ActingMembershipID, &e.Action, &e.ResourceType, &e.ResourceID, &e.Outcome, &e.Reason, &e.OccurredAt); err != nil {
			rows.Close()
			return AuditEventPage{}, err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return AuditEventPage{}, err
	}
	at := s.currentTime()
	if err = check(at); err != nil {
		return AuditEventPage{}, err
	}
	page := AuditEventPage{Events: events}
	if len(events) > limit {
		page.Events = events[:limit]
		last := page.Events[limit-1]
		raw, _ := json.Marshal(auditCursor{TenantID: strings.ToLower(id.TenantID), Action: action, Outcome: outcome, ActorUserID: actorUserID, From: filter.From, Until: filter.Until, At: last.OccurredAt.UTC().Format(time.RFC3339Nano), ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err = audit(ctx, tx, id, queryAction, "tenant", id.TenantID, "allow", "listed_audit_events", at); err != nil {
		return AuditEventPage{}, errors.Join(ErrAuditUnavailable, err)
	}
	// Never commit an allow event if authorization expired while audit I/O was pending.
	grant, err := s.resolve(ctx, tx, id, s.currentTime())
	if err != nil {
		return AuditEventPage{}, err
	}
	if !grant.all {
		return AuditEventPage{}, ErrNotFound
	}
	if err = tx.Commit(ctx); err != nil {
		return AuditEventPage{}, err
	}
	return page, nil
}
