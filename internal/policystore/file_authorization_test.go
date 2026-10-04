package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func TestFileReservationGroupPairAndCurrentMembership(t *testing.T) {
	c := db(t)
	seed(t, c)
	seedThirdGroupMember(t, c)
	runtimePolicyEdit(t, c)
	ctx := context.Background()
	s := policystore.Service{DB: c}
	g, e := s.CreateGroup(ctx, publisher(), createGroupRequest(targetM2, groupMemberC))
	if e != nil {
		t.Fatal(e)
	}
	p := reservationParams()
	p.ConversationID = g.ID
	a, e := s.ReserveFile(ctx, publisher(), p)
	if e != nil {
		t.Fatal(e)
	}
	// Denial between the other two members must block the owner's upload as well.
	run(t, c, "INSERT INTO policy_versions(tenant_id,version,status,published_by_user_id,reason) VALUES($1,1,'draft',$2,'files policy')", tenantA, adminA)
	run(t, c, `INSERT INTO policy_rules(tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,source_membership_id,target_membership_id,reason,effective_from) VALUES($1,1,'peer-deny','hard_deny','send_message',$4,$4,$2,$3,'peer deny','2020-01-01')`, tenantA, targetM2, groupMemberC, orgA)
	run(t, c, "UPDATE policy_versions SET status='published',published_at=clock_timestamp() WHERE tenant_id=$1", tenantA)
	run(t, c, "INSERT INTO policy_current(tenant_id,current_version) VALUES($1,1)", tenantA)
	if _, e = s.ReserveFile(ctx, publisher(), p); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal("pair policy bypass", e)
	}
	if _, e = s.GetOwnFile(ctx, publisher(), a.File.ID); e != nil {
		t.Fatal("own status must not require send", e)
	}
	run(t, c, "UPDATE conversation_membership_intervals SET status='left',leave_seq=1,left_at=clock_timestamp() WHERE tenant_id=$1 AND conversation_id=$2 AND user_id=$3", tenantA, g.ID, adminA)
	if _, e = s.GetOwnFile(ctx, publisher(), a.File.ID); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal("former member", e)
	}
}
func TestFileReservationDirectReversePolicy(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	ctx := context.Background()
	run(t, c, "INSERT INTO policy_versions(tenant_id,version,status,published_by_user_id,reason) VALUES($1,1,'draft',$2,'reverse')", tenantA, adminA)
	run(t, c, `INSERT INTO policy_rules(tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,source_membership_id,target_membership_id,reason,effective_from) VALUES($1,1,'reverse','hard_deny','send_message',$4,$4,$2,$3,'reverse deny','2020-01-01')`, tenantA, targetM2, adminM, orgA)
	run(t, c, "UPDATE policy_versions SET status='published',published_at=clock_timestamp() WHERE tenant_id=$1", tenantA)
	run(t, c, "INSERT INTO policy_current(tenant_id,current_version) VALUES($1,1)", tenantA)
	if _, e := (policystore.Service{DB: c}).ReserveFile(ctx, publisher(), reservationParams()); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
}
func TestFileReservationConfigLimits(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	s := policystore.Service{DB: c}
	ctx := context.Background()
	p := reservationParams()
	if _, e := s.ReserveFile(ctx, publisher(), p); !errors.Is(e, files.ErrUploadDisabled) {
		t.Fatal(e)
	}
	runtimePolicyEdit(t, c)
	p.DeclaredMediaType = "application/zip"
	if _, e := s.ReserveFile(ctx, publisher(), p); !errors.Is(e, files.ErrFileTypeNotAllowed) {
		t.Fatal(e)
	}
	p = reservationParams()
	p.DeclaredSizeBytes = 2
	run(t, c, "UPDATE tenant_file_upload_policy SET max_size_bytes=1 WHERE tenant_id=$1", tenantA)
	if _, e := s.ReserveFile(ctx, publisher(), p); !errors.Is(e, files.ErrFileTooLarge) {
		t.Fatal(e)
	}
	p = reservationParams()
	p.UploaderMembershipID = targetM2
	if _, e := s.ReserveFile(ctx, publisher(), p); !errors.Is(e, files.ErrInvalidIdentity) {
		t.Fatal(e)
	}
}
func TestFileReservationConcurrentSameRequest(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	peer := filePeer(t, c)
	ctx := context.Background()
	type result struct {
		r files.Reservation
		e error
	}
	ch := make(chan result, 2)
	go func() {
		r, e := (policystore.Service{DB: c}).ReserveFile(ctx, publisher(), reservationParams())
		ch <- result{r, e}
	}()
	go func() {
		r, e := (policystore.Service{DB: peer}).ReserveFile(ctx, publisher(), reservationParams())
		ch <- result{r, e}
	}()
	a, b := <-ch, <-ch
	if a.e != nil || b.e != nil || a.r.File.ID != b.r.File.ID || a.r.Duplicate == b.r.Duplicate {
		t.Fatal(a, b)
	}
	var n int
	if e := c.QueryRow(ctx, "SELECT (SELECT count(*) FROM file_objects)+(SELECT count(*) FROM file_lifecycle_events)+(SELECT count(*) FROM audit_events WHERE action='file_reserve')").Scan(&n); e != nil || n != 3 {
		t.Fatal(n, e)
	}
}
func TestFileMetadataForeignStateOracleAndMembership(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	ctx := context.Background()
	s := policystore.Service{DB: c}
	own, e := s.ReserveFile(ctx, publisher(), reservationParams())
	if e != nil {
		t.Fatal(e)
	}
	for _, state := range []string{"allocated", "uploaded"} {
		m := storedFile(t, c, state)
		for _, id := range []access.TrustedIdentity{groupMemberIdentity(), {TenantID: tenantB, UserID: personB, ActingMembershipID: otherM}} {
			if _, e = s.GetOwnFile(ctx, id, m.ID); !errors.Is(e, files.ErrFileNotFound) {
				t.Fatal(state, id, e)
			}
		}
	}
	otherID := "00000000-0000-4000-8000-000000009999"
	run(t, c, "INSERT INTO user_organizations(id,tenant_id,user_id,organization_id,effective_from) VALUES($1,$2,$3,$4,'2020-01-01')", otherID, tenantA, adminA, orgA2)
	id := publisher()
	id.ActingMembershipID = otherID
	if _, e = s.GetOwnFile(ctx, id, own.File.ID); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal("other appointment", e)
	}
	run(t, c, "UPDATE conversations SET status='ended' WHERE id=$1", directA)
	if _, e = s.GetOwnFile(ctx, publisher(), own.File.ID); !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal("ended conversation", e)
	}
}
func TestFileReservationPolicyExpiresDuringWait(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	ctx := context.Background()
	run(t, c, "UPDATE conversations SET direct_high_membership_id=$2 WHERE id=$1", directA, targetM)
	run(t, c, "INSERT INTO policy_versions(tenant_id,version,status,published_by_user_id,reason) VALUES($1,1,'draft',$2,'temporary allow')", tenantA, adminA)
	run(t, c, `INSERT INTO policy_rules(tenant_id,version,rule_id,effect,action,source_organization_id,target_organization_id,bidirectional,requested_by_user_id,approved_by_user_id,reason,effective_from,effective_to) VALUES($1,1,'allow','allow','send_message',$2,$3,true,$4,$4,'temporary','2020-01-01',clock_timestamp()+interval '2 seconds')`, tenantA, orgA, orgA2, adminA)
	run(t, c, "UPDATE policy_versions SET status='published',published_at=clock_timestamp() WHERE tenant_id=$1", tenantA)
	run(t, c, "INSERT INTO policy_current(tenant_id,current_version) VALUES($1,1)", tenantA)
	peer := filePeer(t, c)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT tenant_id FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE", tenantA); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() {
		_, e := (policystore.Service{DB: peer}).ReserveFile(ctx, publisher(), reservationParams())
		ch <- e
	}()
	time.Sleep(2300 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
}
func TestFileReservationBlockedDuringConversationWait(t *testing.T) {
	c := db(t)
	seedDirectConversation(t, c)
	runtimePolicyEdit(t, c)
	peer := filePeer(t, c)
	ctx := context.Background()
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT id FROM conversations WHERE id=$1 FOR UPDATE", directA); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() {
		_, e := (policystore.Service{DB: peer}).ReserveFile(ctx, publisher(), reservationParams())
		ch <- e
	}()
	time.Sleep(100 * time.Millisecond)
	if _, e = tx.Exec(ctx, "UPDATE conversations SET status='policy_blocked' WHERE id=$1", directA); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrFileNotFound) {
		t.Fatal(e)
	}
}
