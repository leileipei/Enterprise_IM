package policystore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/leileipei/Enterprise_IM/internal/files"
)

var fileBusinessRuntimeTables = []string{
	"tenants", "users", "user_organizations", "organizations", "legal_entities", "departments", "user_departments", "admin_grants",
	"conversations", "conversation_membership_intervals", "conversation_legal_holds", "conversation_legal_hold_events",
	"policy_current", "policy_rules", "policy_versions", "policy_decision_events", "tenant_retention_policy_history",
	"messages", "message_idempotency", "message_rate_windows", "outbox_events", "audit_events",
	"file_objects", "file_lifecycle_events", "file_upload_attempts", "file_scan_jobs", "file_worker_audit_events",
	"message_attachments", "file_download_sessions", "file_download_terminal_events",
	"tenant_file_upload_policy", "tenant_file_upload_policy_history", "tenant_file_retention_policy", "tenant_file_retention_policy_history",
	"file_delete_jobs", "file_delete_versions",
}

var fileAuditRuntimeTables = []string{
	"conversations", "file_objects", "file_lifecycle_events", "messages", "message_attachments",
	"users", "user_organizations", "file_download_sessions", "file_download_terminal_events", "file_worker_audit_events", "audit_events",
}

// Updates on read-only facts are needed by PostgreSQL FOR SHARE/UPDATE row locks.
var fileBusinessWritePrivileges = map[string][]string{
	"tenants": {"UPDATE"}, "users": {"UPDATE"}, "user_organizations": {"UPDATE"}, "organizations": {"UPDATE"}, "legal_entities": {"UPDATE"},
	"policy_current": {"UPDATE"}, "conversations": {"UPDATE"}, "conversation_membership_intervals": {"UPDATE"},
	"messages": {"INSERT", "UPDATE"}, "message_idempotency": {"INSERT", "UPDATE"}, "message_rate_windows": {"INSERT", "UPDATE"},
	"outbox_events": {"INSERT"}, "audit_events": {"INSERT"}, "policy_decision_events": {"INSERT"},
	"file_objects": {"UPDATE"}, "message_attachments": {"INSERT", "UPDATE"},
	"file_download_sessions": {"INSERT", "UPDATE"}, "file_download_terminal_events": {"INSERT"},
	"tenant_file_upload_policy": {"UPDATE"}, "tenant_file_upload_policy_history": {"INSERT"},
	"tenant_file_retention_policy": {"UPDATE"}, "tenant_file_retention_policy_history": {"INSERT"},
}
var fileAuditWritePrivileges = map[string][]string{
	"conversations": {"UPDATE"}, "file_objects": {"UPDATE"}, "file_download_sessions": {"UPDATE"},
	"file_download_terminal_events": {"INSERT", "UPDATE"}, "file_worker_audit_events": {"INSERT"},
}

func (s Service) CheckFileBusinessRuntime(ctx context.Context) error {
	return s.checkFileRuntime(ctx, fileBusinessRuntimeTables, fileBusinessWritePrivileges, []string{"audit_events_id_seq", "policy_decision_events_id_seq"})
}

// CheckFileBusinessUploadRuntime checks only the additional permissions needed
// by API reservation and PUT. Scanner/recovery worker grants are independent.
func (s Service) CheckFileBusinessUploadRuntime(ctx context.Context) error {
	return s.checkFileRuntime(ctx,
		[]string{"file_objects", "file_lifecycle_events", "file_upload_attempts", "tenant_file_upload_policy"},
		map[string][]string{
			"file_objects": {"INSERT", "UPDATE"}, "file_lifecycle_events": {"INSERT"},
			"file_upload_attempts": {"INSERT", "UPDATE"}, "tenant_file_upload_policy": {"UPDATE"},
		}, nil)
}
func (s Service) CheckFileDownloadAuditRuntime(ctx context.Context) error {
	return s.checkFileRuntime(ctx, fileAuditRuntimeTables, fileAuditWritePrivileges, []string{"file_worker_audit_events_id_seq"})
}

func (s Service) checkFileRuntime(ctx context.Context, tables []string, writes map[string][]string, sequences []string) error {
	fail := files.ErrDependencyUnavailable
	if s.DB == nil || ctx.Err() != nil {
		return fail
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return fail
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(ctx, "SET TRANSACTION READ ONLY"); e != nil {
		return fail
	}
	var schema string
	if e = tx.QueryRow(ctx, "SELECT n.nspname FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace WHERE r.oid=to_regclass('file_objects')").Scan(&schema); e != nil {
		return fail
	}
	selected := map[string]bool{}
	for _, table := range tables {
		selected[table] = true
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace WHERE r.oid=to_regclass($1) AND n.nspname=$2 AND r.relkind='r') AND has_table_privilege(to_regclass($1),'SELECT')`, table, schema).Scan(&valid); e != nil || !valid {
			return fail
		}
		for _, privilege := range writes[table] {
			if e = tx.QueryRow(ctx, "SELECT has_table_privilege(to_regclass($1),$2)", table, privilege).Scan(&valid); e != nil || !valid {
				return fail
			}
		}
	}
	for _, sequence := range sequences {
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace WHERE r.oid=to_regclass($1) AND n.nspname=$2 AND r.relkind='S') AND (has_sequence_privilege(to_regclass($1),'USAGE') OR has_sequence_privilege(to_regclass($1),'UPDATE'))`, sequence, schema).Scan(&valid); e != nil || !valid {
			return fail
		}
	}
	var functionsExecutable bool
	if e = tx.QueryRow(ctx, `SELECT COALESCE(bool_and(has_function_privilege(p.oid,'EXECUTE')),true)
FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace
JOIN pg_depend d ON d.classid='pg_constraint'::regclass AND d.objid=c.oid AND d.refclassid='pg_proc'::regclass
JOIN pg_proc p ON p.oid=d.refobjid
WHERE n.nspname=$1 AND r.relname=ANY($2::text[])`, schema, tables).Scan(&functionsExecutable); e != nil || !functionsExecutable {
		return fail
	}
	rows, e := tx.Query(ctx, fileRuntimeCatalogSQL, schema, tables)
	if e != nil {
		return fail
	}
	got := map[string]string{}
	for rows.Next() {
		var key, value string
		if e = rows.Scan(&key, &value); e != nil {
			rows.Close()
			return fail
		}
		value = normalizeFileRuntimeDefinition(value, schema)
		if strings.HasPrefix(key, "trigger:") || strings.HasPrefix(key, "function:") {
			value = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
		}
		got[key] = value
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return fail
	}
	for key, expected := range fileBusinessSchemaContracts {
		parts := strings.SplitN(key, ":", 3)
		if len(parts) != 3 {
			return fail
		}
		if selected[parts[1]] && got[key] != expected {
			return fail
		}
	}
	if ctx.Err() != nil {
		return fail
	}
	if tx.Commit(ctx) != nil {
		return fail
	}
	return nil
}

func normalizeFileRuntimeDefinition(s, schema string) string {
	s = strings.ReplaceAll(s, strconv.Quote(schema)+".", "")
	s = strings.ReplaceAll(s, schema+".", "")
	return strings.TrimSpace(s)
}

const fileRuntimeCatalogSQL = `SELECT 'column:'||r.relname||':'||a.attname,
 format_type(a.atttypid,a.atttypmod)||'|'||a.attnotnull::text||'|'||a.attgenerated::text||'|'||coalesce(pg_get_expr(d.adbin,d.adrelid),'')
FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace JOIN pg_attribute a ON a.attrelid=r.oid
LEFT JOIN pg_attrdef d ON d.adrelid=r.oid AND d.adnum=a.attnum
WHERE n.nspname=$1 AND r.relname=ANY($2::text[]) AND r.relkind='r' AND a.attnum>0 AND NOT a.attisdropped
UNION ALL
SELECT 'constraint:'||r.relname||':'||c.conname,
 c.contype::text||'|'||c.convalidated::text||'|'||c.condeferrable::text||'|'||c.condeferred::text||'|'||pg_get_constraintdef(c.oid,true)
FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace
WHERE n.nspname=$1 AND r.relname=ANY($2::text[])
UNION ALL
SELECT 'trigger:'||r.relname||':'||t.tgname,
 t.tgenabled::text||'|'||pg_get_triggerdef(t.oid,true)||'|'||pn.nspname||'.'||p.proname||'|'||pg_get_functiondef(p.oid)
FROM pg_trigger t JOIN pg_class r ON r.oid=t.tgrelid JOIN pg_namespace n ON n.oid=r.relnamespace
JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace pn ON pn.oid=p.pronamespace
WHERE n.nspname=$1 AND r.relname=ANY($2::text[]) AND NOT t.tgisinternal
UNION ALL
SELECT DISTINCT 'function:'||r.relname||':'||p.proname||'('||pg_get_function_identity_arguments(p.oid)||')',
 pn.nspname||'.'||p.proname||'|'||pg_get_functiondef(p.oid)
FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace
JOIN pg_depend d ON d.classid='pg_constraint'::regclass AND d.objid=c.oid AND d.refclassid='pg_proc'::regclass
JOIN pg_proc p ON p.oid=d.refobjid JOIN pg_namespace pn ON pn.oid=p.pronamespace
WHERE n.nspname=$1 AND r.relname=ANY($2::text[]) AND pn.nspname<>'pg_catalog'
UNION ALL
SELECT 'index:'||r.relname||':'||ir.relname,
 i.indisvalid::text||'|'||i.indisready::text||'|'||pg_get_indexdef(i.indexrelid)
FROM pg_index i JOIN pg_class r ON r.oid=i.indrelid JOIN pg_namespace n ON n.oid=r.relnamespace
JOIN pg_class ir ON ir.oid=i.indexrelid
WHERE n.nspname=$1 AND r.relname=ANY($2::text[]) AND i.indisunique
`
