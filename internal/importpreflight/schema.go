package importpreflight

import "strings"

// Names and order are part of the public diagnostic contract.
type Entity string
type Field string
type Code string
type Status string

const (
	MaxInput  = 10 * 1024 * 1024
	MaxDepth  = 16
	MaxRows   = 10000
	MaxString = 4096
	MaxIssues = 200
	MaxReport = 256 * 1024
)

var entities = []Entity{"tenants", "legal_entities", "organizations", "departments", "users", "user_organizations", "user_departments", "external_identities", "admin_grants"}
var fields = map[Entity][]Field{
	"tenants":             {"id", "code", "name", "status"},
	"legal_entities":      {"id", "tenant_id", "code", "name", "status"},
	"organizations":       {"id", "tenant_id", "parent_id", "legal_entity_id", "org_type", "code", "name", "status"},
	"departments":         {"id", "tenant_id", "organization_id", "parent_id", "code", "name", "status"},
	"users":               {"id", "tenant_id", "global_employee_no", "display_name", "status"},
	"user_organizations":  {"id", "tenant_id", "user_id", "organization_id", "employee_no", "title", "effective_from", "effective_to", "is_primary", "status"},
	"user_departments":    {"id", "tenant_id", "user_organization_id", "organization_id", "department_id", "effective_from", "effective_to", "is_primary", "status"},
	"external_identities": {"issuer", "subject", "tenant_id", "user_id", "status"},
	"admin_grants":        {"id", "tenant_id", "membership_id", "membership_organization_id", "role", "scope_organization_id", "status", "effective_from", "effective_to"},
}
var headerFields = []Field{"format_version", "baseline_commit", "data_origin", "reference_time", "identity_source_selected", "tables"}

func entityRank(e Entity) int {
	if e == "document" {
		return 0
	}
	for i, v := range entities {
		if v == e {
			return i + 1
		}
	}
	return len(entities) + 1
}
func fieldRank(e Entity, f Field) int {
	fs := fields[e]
	if e == "document" {
		fs = headerFields
	}
	for i, v := range fs {
		if v == f {
			return i
		}
	}
	for i, v := range []Field{"_unknown", "_record", "_document"} {
		if v == f {
			return len(fs) + i
		}
	}
	return len(fs) + 3
}
func knownEntity(s string) (Entity, bool) {
	for _, e := range entities {
		if string(e) == s {
			return e, true
		}
	}
	return "", false
}
func knownField(e Entity, s string) (Field, bool) {
	fs := fields[e]
	if e == "document" {
		fs = headerFields
	}
	for _, f := range fs {
		if string(f) == s {
			return f, true
		}
	}
	return "", false
}

type fieldSpec struct {
	kind        string
	nullable    bool
	defaultText string
	defaultBool bool
	hasDefault  bool
	enums       []string
	nonblank    bool
}

func specification(e Entity, f Field) fieldSpec {
	s := fieldSpec{kind: "text"}
	if f == "id" || strings.HasSuffix(string(f), "_id") {
		s.kind = "uuid"
	}
	switch f {
	case "parent_id", "legal_entity_id", "scope_organization_id", "employee_no", "title", "effective_to":
		s.nullable = true
	}
	if f == "effective_from" || f == "effective_to" {
		s.kind = "time"
	}
	if f == "is_primary" {
		s.kind = "bool"
		s.hasDefault = true
	}
	if f == "issuer" || f == "subject" {
		s.nonblank = true
	}
	switch f {
	case "status":
		s.hasDefault = true
		s.defaultText = "active"
		switch e {
		case "tenants":
			s.enums = []string{"active", "suspended"}
		case "users":
			s.enums = []string{"active", "frozen", "departed"}
		case "user_organizations", "user_departments":
			s.enums = []string{"active", "suspended", "ended"}
		case "admin_grants":
			s.enums = []string{"active", "revoked"}
		default:
			s.enums = []string{"active", "disabled"}
		}
	case "org_type":
		s.enums = []string{"virtual_group", "headquarters", "company", "branch", "division", "overseas"}
	case "role":
		s.enums = []string{"group_admin", "organization_admin"}
	}
	return s
}
