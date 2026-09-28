// Package policy evaluates communication boundaries between two validated memberships.
// Authentication, resource authorization and group pairwise checks remain caller responsibilities.
package policy

import (
	"slices"
	"time"
)

type Action string

const (
	ActionDirectoryView Action = "directory_view"
	ActionStartChat     Action = "start_chat"
	ActionSendMessage   Action = "send_message"
	ActionCreateGroup   Action = "create_group"
	ActionInviteGroup   Action = "invite_group"
)

type Effect string

const (
	EffectHardDeny       Effect = "hard_deny"
	EffectIsolate        Effect = "isolate"
	EffectAllow          Effect = "allow"
	EffectExceptionAllow Effect = "exception_allow"
)

type Reason string

const (
	ReasonInvalidContext             Reason = "invalid_context"
	ReasonTenantMismatch             Reason = "tenant_mismatch"
	ReasonInactiveIdentity           Reason = "inactive_identity"
	ReasonScopeDenied                Reason = "scope_denied"
	ReasonResourceInactive           Reason = "resource_inactive"
	ReasonHardDeny                   Reason = "hard_deny"
	ReasonIsolated                   Reason = "isolated"
	ReasonCrossOrganizationDenied    Reason = "cross_organization_denied"
	ReasonCrossLegalApprovalRequired Reason = "cross_legal_approval_required"
	ReasonAllowedSameOrganization    Reason = "allowed_same_organization"
	ReasonAllowedRule                Reason = "allowed_rule"
	ReasonAllowedException           Reason = "allowed_exception"
)

type Membership struct {
	ID             string
	TenantID       string
	OrganizationID string
	LegalEntityID  string
	AccountStatus  string
	Status         string
	EffectiveFrom  time.Time
	EffectiveTo    time.Time
}

func (m Membership) activeAt(at time.Time) bool {
	return m.AccountStatus == "active" && m.Status == "active" &&
		!m.EffectiveFrom.IsZero() && !at.Before(m.EffectiveFrom) &&
		(m.EffectiveTo.IsZero() || at.Before(m.EffectiveTo))
}

type Rule struct {
	ID                   string
	TenantID             string
	Effect               Effect
	Action               Action
	SourceOrganizationID string
	TargetOrganizationID string
	SourceMembershipID   string
	TargetMembershipID   string
	Bidirectional        bool
	OverrideRuleID       string
	RequestedBy          string
	ApprovedBy           string
	Reason               string
	CrossLegalApproved   bool
	EffectiveFrom        time.Time
	EffectiveTo          time.Time
}

func (r Rule) activeAt(at time.Time) bool {
	return !r.EffectiveFrom.IsZero() && !at.Before(r.EffectiveFrom) &&
		(r.EffectiveTo.IsZero() || at.Before(r.EffectiveTo))
}

func (r Rule) matches(in Input) bool {
	if r.ID == "" || r.TenantID != in.Actor.TenantID || r.Action != in.Action || !r.activeAt(in.At) {
		return false
	}
	if r.directedMatch(in.Actor, in.Target) {
		return true
	}
	return r.Bidirectional && r.directedMatch(in.Target, in.Actor)
}

func (r Rule) directedMatch(source, target Membership) bool {
	return (r.SourceOrganizationID == "" || r.SourceOrganizationID == source.OrganizationID) &&
		(r.TargetOrganizationID == "" || r.TargetOrganizationID == target.OrganizationID) &&
		(r.SourceMembershipID == "" || r.SourceMembershipID == source.ID) &&
		(r.TargetMembershipID == "" || r.TargetMembershipID == target.ID)
}

func (r Rule) validException() bool {
	return r.Effect == EffectExceptionAllow && r.OverrideRuleID != "" &&
		r.SourceMembershipID != "" && r.TargetMembershipID != "" &&
		r.ApprovedBy != "" &&
		!r.EffectiveTo.IsZero() && r.EffectiveTo.After(r.EffectiveFrom)
}

func supportedAction(action Action) bool {
	switch action {
	case ActionDirectoryView, ActionStartChat, ActionSendMessage, ActionCreateGroup, ActionInviteGroup:
		return true
	default:
		return false
	}
}

type Input struct {
	Action         Action
	Actor          Membership
	Target         Membership
	At             time.Time
	ScopeAllowed   bool
	ResourceActive bool
	PolicyVersion  int64
	Rules          []Rule
}

type Decision struct {
	Allowed           bool
	Reason            Reason
	PolicyVersion     int64
	MatchedRuleIDs    []string
	OverriddenRuleIDs []string
}

// Evaluate is order independent and fails closed when context or required approval is missing.
func Evaluate(in Input) Decision {
	denied := func(reason Reason, ids ...string) Decision {
		return Decision{Reason: reason, PolicyVersion: in.PolicyVersion, MatchedRuleIDs: ids}
	}
	if in.At.IsZero() || !supportedAction(in.Action) || in.Actor.ID == "" || in.Target.ID == "" ||
		in.Actor.TenantID == "" || in.Target.TenantID == "" ||
		in.Actor.OrganizationID == "" || in.Target.OrganizationID == "" ||
		in.Actor.LegalEntityID == "" || in.Target.LegalEntityID == "" {
		return denied(ReasonInvalidContext)
	}
	if in.Actor.TenantID != in.Target.TenantID {
		return denied(ReasonTenantMismatch)
	}
	if !in.Actor.activeAt(in.At) || !in.Target.activeAt(in.At) {
		return denied(ReasonInactiveIdentity)
	}
	if !in.ScopeAllowed {
		return denied(ReasonScopeDenied)
	}
	if !in.ResourceActive {
		return denied(ReasonResourceInactive)
	}

	var isolates, allows, exceptions []Rule
	var hardDenies []string
	for _, rule := range in.Rules {
		if !rule.matches(in) {
			continue
		}
		switch rule.Effect {
		case EffectHardDeny:
			hardDenies = append(hardDenies, rule.ID)
		case EffectIsolate:
			isolates = append(isolates, rule)
		case EffectAllow:
			if rule.ApprovedBy != "" {
				allows = append(allows, rule)
			}
		case EffectExceptionAllow:
			if rule.validException() {
				exceptions = append(exceptions, rule)
			}
		}
	}
	if len(hardDenies) > 0 {
		slices.Sort(hardDenies)
		return denied(ReasonHardDeny, hardDenies...)
	}

	var covered, usedExceptions []string
	var usedExceptionRules []Rule
	for _, isolation := range isolates {
		var covering *Rule
		for _, exception := range exceptions {
			if exception.OverrideRuleID == isolation.ID {
				covering = &exception
				break
			}
		}
		if covering == nil {
			return denied(ReasonIsolated, isolation.ID)
		}
		covered = append(covered, isolation.ID)
		usedExceptions = append(usedExceptions, covering.ID)
		usedExceptionRules = append(usedExceptionRules, *covering)
	}

	crossOrganization := in.Actor.OrganizationID != in.Target.OrganizationID
	crossLegalGroup := (in.Action == ActionCreateGroup || in.Action == ActionInviteGroup) &&
		in.Actor.LegalEntityID != in.Target.LegalEntityID
	if crossLegalGroup {
		approved := false
		for _, rule := range append(slices.Clone(allows), usedExceptionRules...) {
			if rule.CrossLegalApproved {
				approved = true
				break
			}
		}
		if !approved {
			return denied(ReasonCrossLegalApprovalRequired)
		}
	}
	if len(usedExceptions) > 0 {
		slices.Sort(covered)
		slices.Sort(usedExceptions)
		return Decision{Allowed: true, Reason: ReasonAllowedException, PolicyVersion: in.PolicyVersion,
			MatchedRuleIDs: usedExceptions, OverriddenRuleIDs: covered}
	}
	if crossOrganization {
		if len(allows) == 0 {
			return denied(ReasonCrossOrganizationDenied)
		}
		ids := make([]string, 0, len(allows))
		for _, rule := range allows {
			ids = append(ids, rule.ID)
		}
		slices.Sort(ids)
		return Decision{Allowed: true, Reason: ReasonAllowedRule, PolicyVersion: in.PolicyVersion, MatchedRuleIDs: ids}
	}
	return Decision{Allowed: true, Reason: ReasonAllowedSameOrganization, PolicyVersion: in.PolicyVersion}
}
