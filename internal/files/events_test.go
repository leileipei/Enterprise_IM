package files

import (
	"errors"
	"testing"
)

func eventFixture(from, to State, reason Reason) Event {
	kind := "worker"
	user, member, job := "", "", "10000000-0000-4000-8000-000000000061"
	version := int64(3)
	if reason == "allocated" || reason == "upload_sealed" {
		kind = "user"
		user = validCreateFixture().UploaderUserID
		member = validCreateFixture().UploaderMembershipID
		job = ""
	}
	if from == "" {
		version = 0
	}
	return Event{TenantID: validCreateFixture().TenantID, FileID: "10000000-0000-4000-8000-00000000000f", StateVersion: version, FromState: from, ToState: to, Reason: reason, OccurredAt: fileTime, ActorKind: kind, ActorUserID: user, ActingMembershipID: member, WorkerJobID: job}
}
func TestFileLifecycleEventOriginAndReason(t *testing.T) {
	initial := eventFixture("", StateAllocated, "allocated")
	if e := ValidateEvent(initial); e != nil {
		t.Fatal(e)
	}
	for pair, reason := range allowedFileEdges {
		e := eventFixture(pair[0], pair[1], reason)
		if err := ValidateEvent(e); err != nil {
			t.Fatalf("valid event %v: %v", pair, err)
		}
	}
	base := eventFixture(StateScanning, StateReady, "scan_clean")
	for _, mut := range []func(*Event){func(e *Event) { e.Reason = "bogus" }, func(e *Event) { e.Reason = "scan_error" }, func(e *Event) { e.FromState = "bogus" }, func(e *Event) { e.ToState = "bogus" }, func(e *Event) { e.StateVersion = -1 }, func(e *Event) { e.StateVersion = 0 }, func(e *Event) { e.ActorKind = "external" }, func(e *Event) { e.ActorUserID = validCreateFixture().UploaderUserID }, func(e *Event) { e.WorkerJobID = "" }, func(e *Event) { e.WorkerJobID = "bad" }, func(e *Event) { e.TenantID = "bad" }, func(e *Event) { e.Reason = "scan_clean\n" }, func(e *Event) {
		e.ActorKind = "user"
		e.WorkerJobID = ""
		e.ActorUserID = validCreateFixture().UploaderUserID
		e.ActingMembershipID = validCreateFixture().UploaderMembershipID
	}} {
		e := base
		mut(&e)
		if err := ValidateEvent(e); !errors.Is(err, ErrInvalidEvent) {
			t.Fatal("invalid evidence accepted", e, err)
		}
	}
	for _, mut := range []func(*Event){func(e *Event) { e.StateVersion = 1 }, func(e *Event) { e.WorkerJobID = "10000000-0000-4000-8000-000000000061" }, func(e *Event) { e.ActingMembershipID = "" }, func(e *Event) {
		e.ActorKind = "worker"
		e.ActorUserID = ""
		e.ActingMembershipID = ""
		e.WorkerJobID = "10000000-0000-4000-8000-000000000061"
	}} {
		e := initial
		mut(&e)
		if err := ValidateEvent(e); !errors.Is(err, ErrInvalidEvent) {
			t.Fatal("invalid creation event accepted", e, err)
		}
	}
}
