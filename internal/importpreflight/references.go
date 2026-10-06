package importpreflight

import "context"

type relationKey struct {
	entity Entity
	row    int
	field  Field
}
type Relations map[relationKey]bool

func (r Relations) Valid(e Entity, row int, f Field) bool { return r[relationKey{e, row, f}] }
func CheckReferences(ctx context.Context, doc Document, idx *Index, c *Collector) (Relations, error) {
	rels := Relations{}
	for _, e := range entities {
		for _, r := range doc.Tables[e] {
			if er := ContextFailure(ctx); er != nil {
				return rels, er
			}
			ref := func(f Field, target Entity, scopes map[Field]Field) {
				key, ok := r.Text(f)
				if !ok {
					return
				}
				for source := range scopes {
					if _, valid := r.Text(source); !valid {
						return
					}
				}
				dest, found := idx.Find(target, key)
				code := Code("")
				if !found {
					code = "REF_NOT_FOUND"
				} else {
					for source, targetField := range scopes {
						a, _ := r.Text(source)
						b, valid := dest.Text(targetField)
						if !valid {
							code = "REF_NOT_FOUND"
							break
						}
						if a != b {
							code = "REF_SCOPE_MISMATCH"
						}
					}
				}
				if code != "" {
					c.Add(Issue{Entity: e, Row: r.Ordinal, Field: f, Code: code})
					return
				}
				rels[relationKey{e, r.Ordinal, f}] = true
			}
			if e != "tenants" {
				ref("tenant_id", "tenants", nil)
			}
			tenant := map[Field]Field{"tenant_id": "tenant_id"}
			orgScope := map[Field]Field{"tenant_id": "tenant_id", "organization_id": "organization_id"}
			switch e {
			case "organizations":
				ref("parent_id", "organizations", tenant)
				ref("legal_entity_id", "legal_entities", tenant)
				kind, valid := r.Text("org_type")
				legal := r.Values["legal_entity_id"]
				if valid && legal.Valid && (legal.IsNull || rels.Valid(e, r.Ordinal, "legal_entity_id")) {
					if (kind == "virtual_group") != legal.IsNull {
						c.Add(Issue{Entity: e, Row: r.Ordinal, Field: "legal_entity_id", Code: "VIRTUAL_LEGAL_MISMATCH"})
					}
				}
			case "departments":
				ref("organization_id", "organizations", tenant)
				ref("parent_id", "departments", orgScope)
			case "user_organizations":
				ref("user_id", "users", tenant)
				ref("organization_id", "organizations", tenant)
				if rels.Valid(e, r.Ordinal, "organization_id") {
					id, _ := r.Text("organization_id")
					org, _ := idx.Find("organizations", id)
					if kind, ok := org.Text("org_type"); ok && kind == "virtual_group" {
						c.Add(Issue{Entity: e, Row: r.Ordinal, Field: "organization_id", Code: "VIRTUAL_MEMBERSHIP"})
					}
				}
			case "user_departments":
				ref("organization_id", "organizations", tenant)
				ref("user_organization_id", "user_organizations", orgScope)
				ref("department_id", "departments", orgScope)
			case "external_identities":
				ref("user_id", "users", tenant)
			case "admin_grants":
				ref("membership_organization_id", "organizations", tenant)
				ref("membership_id", "user_organizations", map[Field]Field{"tenant_id": "tenant_id", "membership_organization_id": "organization_id"})
				ref("scope_organization_id", "organizations", tenant)
				role, valid := r.Text("role")
				scope := r.Values["scope_organization_id"]
				if valid && scope.Valid && (scope.IsNull || rels.Valid(e, r.Ordinal, "scope_organization_id")) {
					if (role == "group_admin") != scope.IsNull {
						c.Add(Issue{Entity: e, Row: r.Ordinal, Field: "scope_organization_id", Code: "GRANT_SCOPE_INVALID"})
					}
				}
			}
		}
	}
	return rels, ContextFailure(ctx)
}
