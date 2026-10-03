package access_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

func TestListLegalHoldsTenantScopeAndCursor(t *testing.T) {
	conn := testDB(t)
	seedLegalHoldService(t, conn)
	admin := access.TrustedIdentity{TenantID: tenantA, UserID: adminA, ActingMembershipID: adminM}
	svc := access.Service{DB: conn, Now: func() time.Time { return fixedTime }}
	ctx := context.Background()
	for i, request := range []string{legalHoldRequestOne, legalHoldRequestTwo, legalHoldPlaceRequestThree} {
		if _, _, err := svc.PlaceLegalHold(ctx, admin, legalHoldConversation, request, []string{"A", "B", "C"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.ListLegalHolds(ctx, admin, legalHoldConversation, "", 2)
	if err != nil || len(first.Holds) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := svc.ListLegalHolds(ctx, admin, legalHoldConversation, first.NextCursor, 2)
	if err != nil || len(second.Holds) != 1 || second.NextCursor != "" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	seen := map[string]bool{}
	for _, hold := range append(first.Holds, second.Holds...) {
		if seen[hold.ID] {
			t.Fatalf("duplicate hold %s", hold.ID)
		}
		seen[hold.ID] = true
	}
	if len(seen) != 3 || first.Holds[0].ID > first.Holds[1].ID ||
		first.Holds[1].ID > second.Holds[0].ID {
		t.Fatalf("unstable tied timestamp order: %+v %+v", first, second)
	}
	for _, input := range []struct {
		identity             access.TrustedIdentity
		conversation, cursor string
	}{
		{admin, legalHoldOtherConversation, first.NextCursor},
		{admin, legalHoldConversation, "bad"},
		{admin, legalHoldConversation, strings.Repeat("a", 2000)},
		{access.TrustedIdentity{TenantID: tenantB, UserID: personB, ActingMembershipID: personBM}, legalHoldConversation, first.NextCursor},
	} {
		if _, err := svc.ListLegalHolds(ctx, input.identity, input.conversation, input.cursor, 2); err == nil {
			t.Fatalf("invalid cursor or scope accepted: %+v", input)
		}
	}
	orgAdmin := access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: personM2}
	if _, err := svc.ListLegalHolds(ctx, orgAdmin, legalHoldConversation, "", 2); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("org admin listed holds: %v", err)
	}
	run(t, conn, `CREATE FUNCTION reject_legal_hold_list_audit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.action='legal_hold_list' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$`)
	run(t, conn, `CREATE TRIGGER reject_legal_hold_list_audit BEFORE INSERT ON audit_events
 FOR EACH ROW EXECUTE FUNCTION reject_legal_hold_list_audit()`)
	page, err := svc.ListLegalHolds(ctx, admin, legalHoldConversation, "", 2)
	if !errors.Is(err, access.ErrAuditUnavailable) || len(page.Holds) != 0 {
		t.Fatalf("audit failure leaked holds: %+v %v", page, err)
	}
}
