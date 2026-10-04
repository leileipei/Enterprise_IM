package policy

import (
	"testing"
	"time"
)

var at = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

func member(id, org, legal string) Membership {
	return Membership{
		ID: id, TenantID: "tenant-a", OrganizationID: org, LegalEntityID: legal,
		AccountStatus: "active", Status: "active", EffectiveFrom: at.Add(-time.Hour),
	}
}

func input(actor, target Membership) Input {
	return Input{
		Action: ActionStartChat, Actor: actor, Target: target,
		At: at, ScopeAllowed: true, ResourceActive: true, PolicyVersion: 7,
	}
}

func isolation() Rule {
	return Rule{
		ID: "isolation-12", TenantID: "tenant-a", Effect: EffectIsolate, Action: ActionStartChat,
		SourceOrganizationID: "org-a", TargetOrganizationID: "org-b", Bidirectional: true,
		EffectiveFrom: at.Add(-time.Hour),
	}
}

func exception() Rule {
	return Rule{
		ID: "project-exception", TenantID: "tenant-a", Effect: EffectExceptionAllow, Action: ActionStartChat,
		SourceMembershipID: "member-a", TargetMembershipID: "member-b",
		SourceOrganizationID: "org-a", TargetOrganizationID: "org-b",
		OverrideRuleID: "isolation-12", ApprovedBy: "admin-1", EffectiveFrom: at.Add(-time.Hour), EffectiveTo: at.Add(time.Hour),
	}
}

func TestSameOrganizationAllowedByDefault(t *testing.T) {
	got := Evaluate(input(member("member-a", "org-a", "legal-a"), member("member-b", "org-a", "legal-a")))
	if !got.Allowed || got.Reason != ReasonAllowedSameOrganization || got.PolicyVersion != 7 {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestHistoryHardDenyIgnoresOrdinaryRulesAndRevokesEitherDirection(t *testing.T) {
	oldActor := member("old-a", "org-a", "legal-a")
	currentActor := member("new-a", "org-c", "legal-a")
	peer := member("peer", "org-b", "legal-a")
	oldActor.Status = "ended"
	peer.Status = "ended"
	ordinary := isolation()
	if HistoryHardDeny(currentActor, oldActor, peer, at, []Rule{ordinary}) {
		t.Fatal("ordinary isolation revoked history")
	}
	directoryHard := ordinary
	directoryHard.Effect = EffectHardDeny
	directoryHard.Action = ActionDirectoryView
	if HistoryHardDeny(currentActor, oldActor, peer, at, []Rule{directoryHard}) {
		t.Fatal("directory-only hard deny revoked message history")
	}
	hard := ordinary
	hard.Effect = EffectHardDeny
	hard.Bidirectional = false
	hard.Action = ActionSendMessage
	if !HistoryHardDeny(currentActor, peer, oldActor, at, []Rule{hard}) {
		t.Fatal("historical directed hard deny missed on reverse read")
	}
	hard.SourceOrganizationID = "org-c"
	if !HistoryHardDeny(currentActor, oldActor, peer, at, []Rule{hard}) {
		t.Fatal("current actor hard deny missed after transfer")
	}
	hard.EffectiveTo = at
	if HistoryHardDeny(currentActor, oldActor, peer, at, []Rule{hard}) {
		t.Fatal("expired hard deny revoked history")
	}
}

func TestCrossOrganizationDeniedWithoutExplicitRule(t *testing.T) {
	got := Evaluate(input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b")))
	if got.Allowed || got.Reason != ReasonCrossOrganizationDenied {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestSpecificExceptionCoversOnlyReferencedIsolation(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	req.Rules = []Rule{isolation(), exception()}
	got := Evaluate(req)
	if !got.Allowed || got.Reason != ReasonAllowedException || len(got.OverriddenRuleIDs) != 1 || got.OverriddenRuleIDs[0] != "isolation-12" {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestWrongOverrideIDCannotBypassIsolation(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	ex := exception()
	ex.OverrideRuleID = "another-isolation"
	req.Rules = []Rule{isolation(), ex}
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonIsolated {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestExpiredExceptionCannotBypassIsolation(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	ex := exception()
	ex.EffectiveTo = at
	req.Rules = []Rule{isolation(), ex}
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonIsolated {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestExceptionCannotCoverHardDeny(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	hard := isolation()
	hard.ID = "hard-1"
	hard.Effect = EffectHardDeny
	req.Rules = []Rule{isolation(), exception(), hard}
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonHardDeny || len(got.MatchedRuleIDs) != 1 || got.MatchedRuleIDs[0] != "hard-1" {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestCrossTenantDeniedBeforeRules(t *testing.T) {
	target := member("member-b", "org-b", "legal-b")
	target.TenantID = "tenant-b"
	req := input(member("member-a", "org-a", "legal-a"), target)
	req.Rules = []Rule{exception()}
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonTenantMismatch {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestExpiredMembershipDeniedEvenWithinSameOrganization(t *testing.T) {
	actor := member("member-a", "org-a", "legal-a")
	actor.EffectiveTo = at
	got := Evaluate(input(actor, member("member-b", "org-a", "legal-a")))
	if got.Allowed || got.Reason != ReasonInactiveIdentity {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestScopeDenialCannotBeOverridden(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-a", "legal-a"))
	req.ScopeAllowed = false
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonScopeDenied {
		t.Fatalf("unexpected decision: %+v", got)
	}
}

func TestCrossLegalGroupRequiresExplicitApproval(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	req.Action = ActionInviteGroup
	allow := Rule{
		ID: "cross-org", TenantID: "tenant-a", Effect: EffectAllow, Action: ActionInviteGroup,
		SourceOrganizationID: "org-a", TargetOrganizationID: "org-b", ApprovedBy: "admin-1", EffectiveFrom: at.Add(-time.Hour),
	}
	req.Rules = []Rule{allow}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonCrossLegalApprovalRequired {
		t.Fatalf("unexpected decision without legal approval: %+v", got)
	}
	allow.CrossLegalApproved = true
	req.Rules = []Rule{allow}
	if got := Evaluate(req); !got.Allowed || got.Reason != ReasonAllowedRule {
		t.Fatalf("unexpected decision with legal approval: %+v", got)
	}
}

func TestCrossLegalGroupDenialRetainsMatchedRuleForAudit(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	req.Action = ActionInviteGroup
	allow := Rule{ID: "cross-org", TenantID: "tenant-a", Effect: EffectAllow, Action: ActionInviteGroup,
		SourceOrganizationID: "org-a", TargetOrganizationID: "org-b", ApprovedBy: "admin-1",
		EffectiveFrom: at.Add(-time.Hour)}
	req.Rules = []Rule{allow}
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonCrossLegalApprovalRequired ||
		len(got.MatchedRuleIDs) != 1 || got.MatchedRuleIDs[0] != "cross-org" {
		t.Fatalf("cross-legal denial lost matched rule: %+v", got)
	}
}

func TestRuleFromOtherTenantCannotAuthorize(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	allow := Rule{
		ID: "other-tenant-allow", TenantID: "tenant-b", Effect: EffectAllow, Action: ActionStartChat,
		SourceOrganizationID: "org-a", TargetOrganizationID: "org-b", ApprovedBy: "admin-1", EffectiveFrom: at.Add(-time.Hour),
	}
	req.Rules = []Rule{allow}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonCrossOrganizationDenied {
		t.Fatalf("other tenant rule affected decision: %+v", got)
	}
}

func TestUnrelatedExceptionCannotApproveCrossLegalGroup(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	req.Action = ActionInviteGroup
	isolate := isolation()
	isolate.Action = ActionInviteGroup
	cover := exception()
	cover.Action = ActionInviteGroup
	unrelated := cover
	unrelated.ID = "unrelated-approval"
	unrelated.OverrideRuleID = "another-isolation"
	unrelated.CrossLegalApproved = true
	req.Rules = []Rule{isolate, cover, unrelated}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonCrossLegalApprovalRequired {
		t.Fatalf("unrelated exception approved group: %+v", got)
	}
}

func TestUnknownActionFailsClosedWithinSameOrganization(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-a", "legal-a"))
	req.Action = Action("delete_all_messages")
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonInvalidContext {
		t.Fatalf("unknown action allowed: %+v", got)
	}
}

func TestPendingExceptionCannotBypassIsolation(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	ex := exception()
	ex.ApprovedBy = ""
	req.Rules = []Rule{isolation(), ex}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonIsolated {
		t.Fatalf("pending exception allowed: %+v", got)
	}
}

func TestHardDenyAuditsAllMatchingRulesInStableOrder(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-a", "legal-a"))
	first := isolation()
	first.ID = "hard-z"
	first.Effect = EffectHardDeny
	first.TargetOrganizationID = "org-a"
	second := first
	second.ID = "hard-a"
	req.Rules = []Rule{first, second}
	got := Evaluate(req)
	if got.Allowed || got.Reason != ReasonHardDeny || len(got.MatchedRuleIDs) != 2 || got.MatchedRuleIDs[0] != "hard-a" || got.MatchedRuleIDs[1] != "hard-z" {
		t.Fatalf("hard denies not stable: %+v", got)
	}
}

func TestExceptionRequiresSpecificMembershipsAndExpiry(t *testing.T) {
	req := input(member("member-a", "org-a", "legal-a"), member("member-b", "org-b", "legal-b"))
	ex := exception()
	ex.TargetMembershipID = ""
	req.Rules = []Rule{isolation(), ex}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonIsolated {
		t.Fatalf("underspecified exception was accepted: %+v", got)
	}
	ex = exception()
	ex.EffectiveTo = time.Time{}
	req.Rules = []Rule{isolation(), ex}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonIsolated {
		t.Fatalf("unbounded exception was accepted: %+v", got)
	}
}

func TestPolicyFileDownloadIndependent(t *testing.T) {
	req := input(member("a", "org-a", "legal-a"), member("b", "org-b", "legal-b"))
	req.Action = Action("file_download")
	req.Rules = []Rule{{ID: "allow-send", TenantID: "tenant-a", Action: ActionSendMessage, Effect: EffectAllow, ApprovedBy: "admin", EffectiveFrom: at.Add(-time.Hour)}}
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonCrossOrganizationDenied {
		t.Fatal("file action missing or send rule leaked", got)
	}
	req.Rules[0].Action = Action("file_download")
	if got := Evaluate(req); !got.Allowed {
		t.Fatal("explicit file rule denied", got)
	}
	req.Rules = append(req.Rules, Rule{ID: "deny-file", TenantID: "tenant-a", Action: Action("file_download"), Effect: EffectHardDeny, EffectiveFrom: at.Add(-time.Hour)})
	if got := Evaluate(req); got.Allowed || got.Reason != ReasonHardDeny {
		t.Fatal("file allow bypassed hard deny", got)
	}
	req.Rules = nil
	req.Target = req.Actor
	if got := Evaluate(req); !got.Allowed {
		t.Fatal("same-org file denied", got)
	}
}
