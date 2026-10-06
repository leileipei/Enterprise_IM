package importpreflight

import (
	"testing"
)

func TestReferencesScopesAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		e      Entity
		f      Field
		target Entity
		idx    int
		code   Code
	}{{"external_identities", "tenant_id", "tenants", 1, "REF_SCOPE_MISMATCH"}, {"departments", "parent_id", "departments", 0, "REF_SCOPE_MISMATCH"}, {"user_organizations", "organization_id", "organizations", 0, "VIRTUAL_MEMBERSHIP"}, {"legal_entities", "tenant_id", "users", 0, "REF_NOT_FOUND"}, {"user_departments", "organization_id", "organizations", 0, "REF_SCOPE_MISMATCH"}, {"admin_grants", "membership_organization_id", "organizations", 0, "REF_SCOPE_MISMATCH"}} {
		d := sampleDocument(t)
		row := 0
		if tc.e == "departments" {
			row = 2
		}
		d.Tables[tc.e][row].Values[tc.f] = d.Tables[tc.target][tc.idx].Values["id"]
		_, _, r := checkModel(t, d)
		if !hasCode(r, tc.code) {
			t.Fatalf("%s %s: %+v", tc.e, tc.f, r.Issues)
		}
	}
	d := sampleDocument(t)
	r := duplicate(&d, "organizations", 0)
	d.Tables["organizations"] = append(d.Tables["organizations"], r)
	d.Tables["user_organizations"][0].Values["organization_id"] = r.Values["id"]
	_, rels, rep := checkModel(t, d)
	if rels.Valid("user_organizations", 1, "organization_id") || !hasCode(rep, "REF_NOT_FOUND") || hasCode(rep, "VIRTUAL_MEMBERSHIP") {
		t.Fatal("ambiguous target causes derived issue")
	}
	d = sampleDocument(t)
	d.Tables["organizations"][0].Values["id"] = Value{}
	_, _, rep = checkModel(t, d)
	if hasCode(rep, "SELF_PARENT") {
		t.Fatal("invalid key used")
	}
}
