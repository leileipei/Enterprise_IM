package policystore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func businessSchemaDB(t *testing.T) *pgx.Conn {
	t.Helper()
	if os.Getenv("IM_TEST_DATABASE_URL") == "" {
		t.Fatal("dedicated PostgreSQL required for runtime contract tests")
	}
	return db(t)
}

// Catches same-name but incorrect source/terminal constraints and weakened guards.
func TestFileBusinessRuntimeSchema(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"valid", ""},
		{"missing_column", "ALTER TABLE file_objects DROP COLUMN original_filename CASCADE"},
		{"wrong_type", "ALTER TABLE file_download_sessions ALTER COLUMN expected_bytes TYPE numeric"},
		{"same_name_wrong_fk", "ALTER TABLE file_worker_audit_events DROP CONSTRAINT file_worker_download_terminal_origin; ALTER TABLE file_worker_audit_events ADD CONSTRAINT file_worker_download_terminal_origin FOREIGN KEY(download_session_id) REFERENCES file_download_sessions(id)"},
		{"disabled_guard", "ALTER TABLE file_download_sessions DISABLE TRIGGER file_download_sessions_guard"},
		{"replica_guard", "ALTER TABLE file_objects ENABLE REPLICA TRIGGER file_objects_guard"},
		{"wrong_function_body", "CREATE OR REPLACE FUNCTION guard_file_download_session() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'"},
		{"wrong_guard_association", "DROP TRIGGER file_download_sessions_guard ON file_download_sessions; CREATE TRIGGER file_download_sessions_guard BEFORE INSERT OR UPDATE OR DELETE ON file_download_sessions FOR EACH ROW EXECUTE FUNCTION guard_file_delete_job()"},
		{"nondeferrable_terminal", "DROP TRIGGER file_download_sessions_terminal_pair ON file_download_sessions; CREATE CONSTRAINT TRIGGER file_download_sessions_terminal_pair AFTER INSERT OR UPDATE ON file_download_sessions NOT DEFERRABLE FOR EACH ROW EXECUTE FUNCTION check_file_download_terminal_pair()"},
		{"unvalidated_fk", "ALTER TABLE file_worker_audit_events DROP CONSTRAINT file_worker_download_terminal_origin; ALTER TABLE file_worker_audit_events ADD CONSTRAINT file_worker_download_terminal_origin FOREIGN KEY(tenant_id,file_id,download_session_id) REFERENCES file_download_terminal_events(tenant_id,file_id,session_id) NOT VALID"},
		{"unvalidated_check", "ALTER TABLE file_download_sessions DROP CONSTRAINT file_download_sessions_expected_bytes_check; ALTER TABLE file_download_sessions ADD CONSTRAINT file_download_sessions_expected_bytes_check CHECK(expected_bytes BETWEEN 1 AND 26214400) NOT VALID"},
		{"missing_unsettled_uniqueness", "DROP INDEX file_download_sessions_unsettled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := businessSchemaDB(t)
			if tc.sql != "" {
				run(t, c, tc.sql)
			}
			e := (policystore.Service{DB: c}).CheckFileBusinessRuntime(context.Background())
			if (e == nil) != (tc.sql == "") {
				t.Fatal("runtime accepted weakened schema or refused valid baseline", e)
			}
		})
	}
}

func runtimeRole(t *testing.T, c *pgx.Conn, repair bool) string {
	t.Helper()
	name := fmt.Sprintf("im_runtime_role_%d", time.Now().UnixNano())
	var schema string
	if e := c.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	run(t, c, "CREATE ROLE "+name)
	run(t, c, "GRANT USAGE ON SCHEMA "+schema+" TO "+name)
	run(t, c, "GRANT SELECT ON ALL TABLES IN SCHEMA "+schema+" TO "+name)
	run(t, c, "GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA "+schema+" TO "+name)
	if repair {
		for _, table := range []string{"file_download_sessions", "file_download_terminal_events", "file_worker_audit_events", "conversations", "file_objects"} {
			run(t, c, "GRANT INSERT,UPDATE ON "+table+" TO "+name)
		}
	} else {
		run(t, c, "GRANT INSERT,UPDATE ON ALL TABLES IN SCHEMA "+schema+" TO "+name)
	}
	t.Cleanup(func() {
		c.Exec(context.Background(), "RESET ROLE")
		c.Exec(context.Background(), "DROP OWNED BY "+name)
		c.Exec(context.Background(), "DROP ROLE "+name)
	})
	return name
}

func TestFileBusinessRuntimePrivileges(t *testing.T) {
	for _, table := range []string{"", "audit_events", "file_download_sessions", "message_attachments"} {
		t.Run("revoke_"+table, func(t *testing.T) {
			c := businessSchemaDB(t)
			role := runtimeRole(t, c, false)
			if table != "" {
				run(t, c, "REVOKE INSERT ON "+table+" FROM "+role)
			}
			run(t, c, "SET ROLE "+role)
			e := (policystore.Service{DB: c}).CheckFileBusinessRuntime(context.Background())
			if (e == nil) != (table == "") {
				t.Fatal("wrong runtime privilege acceptance", e)
			}
		})
	}
	t.Run("audit_sequence", func(t *testing.T) {
		c := businessSchemaDB(t)
		role := runtimeRole(t, c, false)
		run(t, c, "REVOKE ALL ON SEQUENCE audit_events_id_seq FROM "+role)
		run(t, c, "SET ROLE "+role)
		if (policystore.Service{DB: c}).CheckFileBusinessRuntime(context.Background()) == nil {
			t.Fatal("missing audit sequence privilege accepted")
		}
	})
}

func TestFileBusinessRuntimeFunctionPrivilege(t *testing.T) {
	c := businessSchemaDB(t)
	role := runtimeRole(t, c, false)
	run(t, c, "REVOKE ALL ON FUNCTION file_metadata_filename_valid(text) FROM PUBLIC")
	run(t, c, "SET ROLE "+role)
	if (policystore.Service{DB: c}).CheckFileBusinessRuntime(context.Background()) == nil {
		t.Fatal("missing validator EXECUTE privilege accepted")
	}
}

func TestFileDownloadAuditRuntimeSchema(t *testing.T) {
	c := businessSchemaDB(t)
	role := runtimeRole(t, c, true)
	run(t, c, "SET ROLE "+role)
	s := policystore.Service{DB: c}
	if e := s.CheckFileDownloadAuditRuntime(context.Background()); e != nil {
		t.Fatal("repair-only least privilege refused", e)
	}
	var before, after int
	if e := c.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM file_download_sessions)+(SELECT count(*) FROM file_worker_audit_events)+(SELECT count(*) FROM audit_events)").Scan(&before); e != nil {
		t.Fatal(e)
	}
	if e := s.CheckFileDownloadAuditRuntime(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := c.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM file_download_sessions)+(SELECT count(*) FROM file_worker_audit_events)+(SELECT count(*) FROM audit_events)").Scan(&after); e != nil || before != after {
		t.Fatal("startup wrote business evidence", e)
	}
	run(t, c, "RESET ROLE")
	run(t, c, "REVOKE UPDATE ON file_download_terminal_events FROM "+role)
	run(t, c, "SET ROLE "+role)
	if s.CheckFileDownloadAuditRuntime(context.Background()) == nil {
		t.Fatal("repair permission bypassed")
	}
}
