package importcompare

import p "github.com/leileipei/Enterprise_IM/internal/importpreflight"

type constraintContract struct {
	kind                  string
	fields                []string
	target                string
	targetFields          []string
	expression, predicate string
	operators             []string
}

func keyConstraint(kind string, fields ...string) constraintContract {
	return constraintContract{kind: kind, fields: fields}
}
func fkConstraint(target string, fields, targetFields []string) constraintContract {
	return constraintContract{kind: "f", fields: fields, target: target, targetFields: targetFields}
}
func profileContracts() map[p.Entity][]constraintContract {
	out := map[p.Entity][]constraintContract{}
	for _, s := range p.Schema() {
		e := s.Entity
		keys := pk(e)
		cols := []string{}
		for _, k := range keys {
			cols = append(cols, string(k))
		}
		out[e] = append(out[e], keyConstraint("p", cols...))
		if fields := businessKeys[e]; len(fields) > 0 {
			cols = []string{}
			for _, k := range fields {
				cols = append(cols, string(k))
			}
			out[e] = append(out[e], keyConstraint("u", cols...))
		}
		if e != "tenants" && e != "external_identities" {
			out[e] = append(out[e], fkConstraint("tenants", []string{"tenant_id"}, []string{"id"}))
		}
	}
	add := func(e p.Entity, target string, source, dest []string) {
		out[e] = append(out[e], fkConstraint(target, source, dest))
	}
	add("organizations", "organizations", []string{"tenant_id", "parent_id"}, []string{"tenant_id", "id"})
	add("organizations", "legal_entities", []string{"tenant_id", "legal_entity_id"}, []string{"tenant_id", "id"})
	add("departments", "organizations", []string{"tenant_id", "organization_id"}, []string{"tenant_id", "id"})
	add("departments", "departments", []string{"tenant_id", "organization_id", "parent_id"}, []string{"tenant_id", "organization_id", "id"})
	add("user_organizations", "users", []string{"tenant_id", "user_id"}, []string{"tenant_id", "id"})
	add("user_organizations", "organizations", []string{"tenant_id", "organization_id"}, []string{"tenant_id", "id"})
	add("user_departments", "user_organizations", []string{"tenant_id", "user_organization_id", "organization_id"}, []string{"tenant_id", "id", "organization_id"})
	add("user_departments", "departments", []string{"tenant_id", "organization_id", "department_id"}, []string{"tenant_id", "organization_id", "id"})
	add("external_identities", "users", []string{"tenant_id", "user_id"}, []string{"tenant_id", "id"})
	add("admin_grants", "user_organizations", []string{"tenant_id", "membership_id", "membership_organization_id"}, []string{"tenant_id", "id", "organization_id"})
	add("admin_grants", "organizations", []string{"tenant_id", "scope_organization_id"}, []string{"tenant_id", "id"})
	for _, e := range []p.Entity{"user_organizations", "user_departments"} {
		person, place := "user_id", "organization_id"
		if e == "user_departments" {
			person, place = "user_organization_id", "department_id"
		}
		for _, primary := range []bool{false, true} {
			fields := []string{"tenant_id", person}
			ops := []string{"pg_catalog.=", "pg_catalog.="}
			predicate := ""
			if !primary {
				fields = append(fields, place)
				ops = append(ops, "pg_catalog.=")
			} else {
				predicate = "is_primary"
			}
			fields = append(fields, "")
			ops = append(ops, "pg_catalog.&&")
			out[e] = append(out[e], constraintContract{kind: "x", fields: fields, expression: "tstzrange(effective_from, effective_to, '[)'::text)", predicate: predicate, operators: ops})
		}
	}
	return out
}
