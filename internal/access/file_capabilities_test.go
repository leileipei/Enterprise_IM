package access_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"testing"
	"time"
)

func TestFileCapabilitiesCurrentIdentity(t *testing.T) {
	c := filePolicyDB(t)
	s := access.Service{DB: c}
	ctx := context.Background()
	ordinary := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	if e := s.ValidateFileIdentity(ctx, ordinary); e != nil {
		t.Fatal("ordinary active employee denied", e)
	}
	for _, id := range []access.TrustedIdentity{{TenantID: tenantB, UserID: personA, ActingMembershipID: personM}, {TenantID: tenantA, UserID: personB, ActingMembershipID: personM}, {TenantID: tenantA, UserID: personA, ActingMembershipID: "bad"}} {
		if e := s.ValidateFileIdentity(ctx, id); !errors.Is(e, access.ErrInvalidIdentity) {
			t.Fatal(e)
		}
	}
	run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp() WHERE id=$1", personM)
	if e := s.ValidateFileIdentity(ctx, ordinary); !errors.Is(e, access.ErrInvalidIdentity) {
		t.Fatal("expired identity accepted", e)
	}
	run(t, c, "UPDATE user_organizations SET effective_to=NULL WHERE id=$1", personM)
	run(t, c, "UPDATE organizations SET status='disabled' WHERE id=$1", orgA)
	if e := s.ValidateFileIdentity(ctx, ordinary); !errors.Is(e, access.ErrInvalidIdentity) {
		t.Fatal("inactive organization accepted", e)
	}
}

func TestFileCapabilitiesCurrentIdentityWaitExpiry(t *testing.T) {
	c := filePolicyDB(t)
	peer := secondConnection(t, c)
	ctx := context.Background()
	ordinary := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM}
	run(t, c, "UPDATE user_organizations SET effective_to=clock_timestamp()+interval '300 milliseconds' WHERE id=$1", personM)
	tx, e := peer.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM user_organizations WHERE id=$1 FOR UPDATE", personM); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- (access.Service{DB: c}).ValidateFileIdentity(ctx, ordinary) }()
	time.Sleep(350 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; !errors.Is(e, access.ErrInvalidIdentity) {
		t.Fatal("lock wait bypassed current expiration", e)
	}
}
