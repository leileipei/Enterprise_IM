package importcompare

import (
	"encoding/hex"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
)

var allowedCodes = map[p.Code]bool{"CANCELED": true, "DATABASE_ACCESS_UNSUPPORTED": true, "DATABASE_CONFIG_INVALID": true, "DATABASE_CONNECT_FAILED": true, "DATABASE_DATA_INVALID": true, "DATABASE_DATA_UNSUPPORTED": true, "DATABASE_LIMIT": true, "DATABASE_PROFILE_UNSUPPORTED": true, "DATABASE_READ_FAILED": true, "DEPARTMENT_INTERVAL_OUTSIDE": true, "DEPTH_LIMIT": true, "DUPLICATE_JSON_KEY": true, "ENUM_INVALID": true, "FIELD_LIMIT": true, "FIELD_REQUIRED": true, "FORMAT_VERSION_UNSUPPORTED": true, "GLOBAL_KEY_CONFLICT": true, "GRANT_SCOPE_INVALID": true, "INPUT_READ_FAILED": true, "INPUT_TOO_LARGE": true, "INPUT_TYPE_UNSUPPORTED": true, "INTERVAL_INVALID": true, "INTERVAL_OVERLAP": true, "INVALID_ENCODING": true, "JSON_INVALID": true, "OUTPUT_WRITE_FAILED": true, "PK_DUPLICATE": true, "PRIMARY_OVERLAP": true, "REF_NOT_FOUND": true, "REF_SCOPE_MISMATCH": true, "ROW_LIMIT": true, "SELF_PARENT": true, "STORED_UNIQUE_CONFLICT": true, "STORED_VALUE_DIFFERS": true, "TARGET_TENANT_NOT_FOUND": true, "TENANT_SELECTION_MISMATCH": true, "TEXT_BLANK": true, "TEXT_NUL": true, "TIMEOUT": true, "TIME_INVALID": true, "TREE_CYCLE": true, "TYPE_INVALID": true, "UNIQUE_DUPLICATE": true, "UNKNOWN_FIELD": true, "UNKNOWN_TABLE": true, "UUID_INVALID": true, "VIRTUAL_LEGAL_MISMATCH": true, "VIRTUAL_MEMBERSHIP": true}

func validateReport(r Report) error {
	bad := func() error { return p.Failure{Code: "OUTPUT_WRITE_FAILED"} }
	statusOK := func(s p.Status) bool { return s == "valid" || s == "invalid" || s == "incomplete" }
	if r.ReportVersion != 1 || r.ValidationProfile != "group_identity_database_v1" || r.Scope != "database_snapshot_insert_compatibility" || !statusOK(r.Status) || !statusOK(r.FileStatus) || r.IdentityProviderChecked || r.AdditionalDatabaseRulesChecked || r.WriteConcurrencyChecked || r.ImportAuthorized || r.ChecksComplete != r.DatabaseChecked || len(r.Counts) != 10 || len(r.ClassificationCounts) != 10 || r.ErrorsTotal < 0 || len(r.Issues) > 200 || r.ErrorsTotal < len(r.Issues) || r.IssuesTruncated != (r.ErrorsTotal > len(r.Issues)) || r.Issues == nil {
		return bad()
	}
	if r.Status == "valid" && (!r.ChecksComplete || r.ErrorsTotal != 0) {
		return bad()
	}
	if r.Status == "incomplete" && r.ChecksComplete {
		return bad()
	}
	if r.DatabaseChecked && (r.FileStatus != "valid" || !r.FileChecksComplete) {
		return bad()
	}
	if r.InputSHA256 != nil {
		if len(*r.InputSHA256) != 64 {
			return bad()
		}
		if _, e := hex.DecodeString(*r.InputSHA256); e != nil {
			return bad()
		}
	}
	totals := [4]int{}
	for _, k := range names() {
		v, ok := r.ClassificationCounts[k]
		n, nok := r.Counts[k]
		if !ok || !nok {
			return bad()
		}
		if n != nil && (*n < 0 || *n > 10000) {
			return bad()
		}
		if !r.DatabaseChecked {
			if v.New != nil || v.Identical != nil || v.Conflict != nil {
				return bad()
			}
			continue
		}
		if v.New == nil || v.Identical == nil || v.Conflict == nil || n == nil || *v.New < 0 || *v.Identical < 0 || *v.Conflict < 0 || *v.New+*v.Identical+*v.Conflict != *n {
			return bad()
		}
		if r.Status == "valid" && *v.Conflict != 0 {
			return bad()
		}
		if k == "total" {
			if totals != [4]int{*n, *v.New, *v.Identical, *v.Conflict} {
				return bad()
			}
		} else {
			totals[0] += *n
			totals[1] += *v.New
			totals[2] += *v.Identical
			totals[3] += *v.Conflict
		}
	}
	seen := map[Issue]bool{}
	for _, i := range r.Issues {
		if i.Stage != "file" && i.Stage != "database" || !allowedCodes[i.Issue.Code] || seen[i] {
			return bad()
		}
		seen[i] = true
		e, f := i.Issue.Entity, i.Issue.Field
		valid := e == "document" && (f == "_document" || f == "_unknown" || f == "_record")
		if e == "document" {
			for _, h := range []p.Field{"format_version", "baseline_commit", "data_origin", "reference_time", "identity_source_selected", "tables"} {
				valid = valid || f == h
			}
		}
		for _, s := range p.Schema() {
			if s.Entity == e {
				valid = f == "_record" || f == "_unknown"
				for _, field := range s.Fields {
					valid = valid || field.Name == f
				}
			}
		}
		if !valid || i.Issue.Row < 0 || i.Issue.Row > 10000 || i.Issue.RelatedRow < 0 || i.Issue.RelatedRow > 10000 {
			return bad()
		}
		if e == "document" && (i.Issue.Row != 0 || i.Issue.RelatedRow != 0) {
			return bad()
		}
	}
	return nil
}
