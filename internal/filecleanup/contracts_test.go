package filecleanup

import (
	"testing"
	"time"
)

func cleanupTicket() Ticket {
	return Ticket{JobID: "00000000-0000-4000-8000-000000000001", TenantID: "00000000-0000-4000-8000-000000000002", FileID: "00000000-0000-4000-8000-000000000003", ConversationID: "00000000-0000-4000-8000-000000000004", OwnerID: "00000000-0000-4000-8000-000000000005", LeaseToken: "00000000-0000-4000-8000-000000000006", StateVersion: 1, LeaseExpiresAt: time.Date(2026, 10, 4, 0, 1, 0, 0, time.UTC)}
}
func TestFileCleanupContractSources(t *testing.T) {
	ticket := cleanupTicket()
	if e := ValidateTicket(ticket); e != nil {
		t.Fatal(e)
	}
	in := Inventory{Versions: []Version{{VersionID: "fixed-v1", AttemptID: "00000000-0000-4000-8000-000000000007"}}, Exhausted: true}
	if e := ValidateInventory(ticket, in); e != nil {
		t.Fatal(e)
	}
	for _, mut := range []func(*Ticket){func(t *Ticket) { t.TenantID = "" }, func(t *Ticket) { t.LeaseToken = "" }, func(t *Ticket) { t.StateVersion = -1 }, func(t *Ticket) { t.PolicyVersion = -1 }, func(t *Ticket) { t.LeaseExpiresAt = time.Time{} }} {
		bad := ticket
		mut(&bad)
		if e := ValidateInventory(bad, in); e == nil {
			t.Fatal("invalid cleanup origin accepted", bad)
		}
	}
	for _, bad := range []Inventory{{Versions: []Version{{VersionID: "null"}}, Exhausted: true}, {Versions: []Version{{VersionID: "fixed-v1", AttemptID: "bad"}}, Exhausted: true}, {Versions: []Version{in.Versions[0], in.Versions[0]}, Exhausted: true}, {Exhausted: true, NextKey: "cursor"}, {Exhausted: false}, {Exhausted: true, Reason: "filename:secret"}} {
		if e := ValidateInventory(ticket, bad); e == nil {
			t.Fatal("invalid inventory accepted", bad)
		}
	}
	c := Commitment{Ticket: ticket, CommitmentID: "00000000-0000-4000-8000-000000000008", VersionID: "fixed-v1"}
	p := AbsenceProof{VersionID: "fixed-v1", CheckedAt: time.Date(2026, 10, 4, 0, 0, 30, 0, time.UTC), Absent: true}
	if e := ValidateAbsenceProof(c, p); e != nil {
		t.Fatal(e)
	}
	p.VersionID = "another-v"
	if e := ValidateAbsenceProof(c, p); e == nil {
		t.Fatal("other version absence accepted")
	}
	p.VersionID = c.VersionID
	p.Absent = false
	if e := ValidateAbsenceProof(c, p); e == nil {
		t.Fatal("present version settled")
	}
}
