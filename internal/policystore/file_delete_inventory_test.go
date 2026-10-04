package policystore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
)

func TestFileDeleteInventoryUnknownAttempt(t *testing.T) {
	c, s, m := deleteCandidateFixture(t, "ready")
	ticket := claimDelete(t, s)
	in := filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: "alien-version", AttemptID: freshFile().ID}}, Exhausted: true}
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, in); !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal(e)
	}
	phase, exhausted, safe, reason := deleteJobFacts(t, c, ticket)
	if phase != "blocked" || exhausted || safe || reason != "unknown_version" {
		t.Fatal(phase, exhausted, safe, reason)
	}
	var n int
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM file_delete_versions WHERE file_id=$1", m.ID).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestFileDeleteInventoryExhaustion(t *testing.T) {
	c, s, m := deleteCandidateFixture(t, "ready")
	ticket := claimDelete(t, s)
	key := "tenants/" + m.TenantID + "/files/" + m.ID
	in := filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: m.ObjectVersionID}}, NextKey: key, NextVersion: m.ObjectVersionID}
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, in); e != nil {
		t.Fatal(e)
	}
	phase, exhausted, safe, reason := deleteJobFacts(t, c, ticket)
	if phase != "inventory" || exhausted || !safe || reason != "inventory_incomplete" {
		t.Fatal(phase, exhausted, safe, reason)
	}
	got, e := s.GetFileDeleteInventory(context.Background(), ticket)
	if e != nil || got.Exhausted || got.NextKey != key || len(got.Versions) != 1 {
		t.Fatal(got, e)
	}
	if e = s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	phase, exhausted, safe, reason = deleteJobFacts(t, c, ticket)
	if phase != "inventory" || !exhausted || !safe || reason != "" {
		t.Fatal(phase, exhausted, safe, reason)
	}
	var stage string
	if e = c.QueryRow(context.Background(), "SELECT phase FROM file_delete_versions WHERE job_id=$1", ticket.JobID).Scan(&stage); e != nil || stage != "inventoried" {
		t.Fatal("inventory granted delete", stage, e)
	}
	bad := ticket
	bad.LeaseToken = freshFile().ID
	if _, e = s.GetFileDeleteInventory(context.Background(), bad); !errors.Is(e, filecleanup.ErrLeaseLost) {
		t.Fatal(e)
	}
}
func TestFileDeleteInventorySealConflict(t *testing.T) {
	c, s, m := deleteCandidateFixture(t, "ready")
	attempt := runtimeAttempt(t, c, m)
	run(t, c, `UPDATE file_upload_attempts SET phase='received',actual_size_bytes=1,sha256=decode(repeat('ab',32),'hex'),detected_media_type='application/pdf' WHERE id=$1`, attempt)
	run(t, c, "UPDATE file_upload_attempts SET phase='storing' WHERE id=$1", attempt)
	run(t, c, "UPDATE file_upload_attempts SET phase='sealed',object_version_id=$2 WHERE id=$1", attempt, m.ObjectVersionID)
	ticket := claimDelete(t, s)
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Versions: []filecleanup.Version{{VersionID: "different", AttemptID: attempt}}, Exhausted: true}); !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal(e)
	}
	_, exhausted, safe, reason := deleteJobFacts(t, c, ticket)
	if exhausted || safe || reason != "unknown_version" {
		t.Fatal(exhausted, safe, reason)
	}
}
func TestFileDeleteInventoryUnknownUpload(t *testing.T) {
	c, s, m := deleteCandidateFixture(t, "allocated")
	attempt := runtimeAttempt(t, c, m)
	run(t, c, `UPDATE file_upload_attempts SET phase='received',actual_size_bytes=1,sha256=decode(repeat('ab',32),'hex'),detected_media_type='application/pdf' WHERE id=$1`, attempt)
	run(t, c, "UPDATE file_upload_attempts SET phase='storing' WHERE id=$1", attempt)
	run(t, c, "UPDATE file_upload_attempts SET phase='recovery_pending' WHERE id=$1", attempt)
	ticket := claimDelete(t, s)
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true}); !errors.Is(e, filecleanup.ErrBlocked) {
		t.Fatal(e)
	}
	_, exhausted, safe, reason := deleteJobFacts(t, c, ticket)
	if exhausted || safe || reason != "unknown_upload" {
		t.Fatal(exhausted, safe, reason)
	}
}

func TestFileDeleteInventoryEmptyStillAccountsForSeal(t *testing.T) {
	_, s, m := deleteCandidateFixture(t, "ready")
	ticket := claimDelete(t, s)
	if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true}); e != nil {
		t.Fatal(e)
	}
	in, e := s.GetFileDeleteInventory(context.Background(), ticket)
	if e != nil || !in.Exhausted || len(in.Versions) != 1 || in.Versions[0].VersionID != m.ObjectVersionID {
		t.Fatal("sealed obligation dropped by empty page", in, e)
	}
}
func TestFileDeleteInventoryMarkerAndCursor(t *testing.T) {
	t.Run("marker", func(t *testing.T) {
		c, s, _ := deleteCandidateFixture(t, "ready")
		ticket := claimDelete(t, s)
		if e := s.RecordFileDeleteInventory(context.Background(), ticket, filecleanup.Inventory{Exhausted: true, Reason: "delete_marker"}); !errors.Is(e, filecleanup.ErrBlocked) {
			t.Fatal(e)
		}
		_, exhausted, safe, reason := deleteJobFacts(t, c, ticket)
		if exhausted || safe || reason != "delete_marker" {
			t.Fatal(exhausted, safe, reason)
		}
	})
	t.Run("repeated cursor", func(t *testing.T) {
		_, s, m := deleteCandidateFixture(t, "ready")
		ticket := claimDelete(t, s)
		in := filecleanup.Inventory{NextKey: "tenants/" + m.TenantID + "/files/" + m.ID, NextVersion: "v1"}
		if e := s.RecordFileDeleteInventory(context.Background(), ticket, in); e != nil {
			t.Fatal(e)
		}
		if e := s.RecordFileDeleteInventory(context.Background(), ticket, in); !errors.Is(e, filecleanup.ErrIncomplete) {
			t.Fatal("nonadvancing persisted cursor accepted", e)
		}
	})
}
func TestFileDeleteInventoryOwnerCAS(t *testing.T) {
	_, s, _ := deleteCandidateFixture(t, "ready")
	old := claimDelete(t, s)
	next := claimDelete(t, s)
	if old.JobID != next.JobID || old.LeaseToken == next.LeaseToken {
		t.Fatal("owner token did not rotate")
	}
	if e := s.RecordFileDeleteInventory(context.Background(), old, filecleanup.Inventory{Exhausted: true}); !errors.Is(e, filecleanup.ErrLeaseLost) {
		t.Fatal("old token mutated inventory", e)
	}
}
