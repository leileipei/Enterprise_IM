package policystore_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestFileFoundationSchemaIsolationAndInput(t *testing.T) {
	c := fileFoundationDB(t)
	seedDirectConversation(t, c)
	for _, tc := range []struct {
		label, column string
		value         any
		code          string
	}{
		{"foreign tenant", "tenant_id", tenantB, "23503"}, {"foreign conversation", "conversation_id", "10000000-0000-4000-8000-000000000099", "23503"}, {"other membership", "uploader_membership_id", targetM, "23503"},
		{"empty name", "original_filename", "", "23514"}, {"long name", "original_filename", strings.Repeat("中", 85) + "a", "23514"}, {"nbsp", "original_filename", "\u00a0x.pdf", "23514"}, {"fullwidth", "original_filename", "x.pdf\u3000", "23514"}, {"C1", "original_filename", "x\u009fy.pdf", "23514"}, {"path", "original_filename", "a/b.pdf", "23514"}, {"backslash", "original_filename", "a\\b.pdf", "23514"}, {"colon", "original_filename", "a:b.pdf", "23514"}, {"dot", "original_filename", "..", "23514"},
		{"MIME uppercase", "declared_media_type", "Application/pdf", "23514"}, {"MIME parameter", "declared_media_type", "application/pdf;charset=utf-8", "23514"}, {"MIME long", "declared_media_type", "a/" + strings.Repeat("b", 126), "23514"}, {"MIME missing subtype", "declared_media_type", "application/", "23514"}, {"zero size", "declared_size_bytes", int64(0), "23514"}, {"negative size", "declared_size_bytes", int64(-1), "23514"}, {"oversize", "declared_size_bytes", int64(26214401), "23514"}, {"digest length", "request_digest", []byte{1}, "23514"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			m := freshFile()
			args := fileArgs(m)
			for i, k := range fileColumns {
				if k == tc.column {
					args[i] = tc.value
				}
			}
			parts := make([]string, len(args))
			for i := range parts {
				parts[i] = "$" + intString(i+1)
			}
			_, e := c.Exec(context.Background(), "INSERT INTO file_objects("+strings.Join(fileColumns, ",")+") VALUES("+strings.Join(parts, ",")+")", args...)
			expectFileSQLState(t, e, tc.code)
		})
	}
	m := freshFile()
	m.OriginalFilename = strings.Repeat("中", 85)
	m.DeclaredSizeBytes = 26214400
	m.DeclaredMediaType = "a/" + strings.Repeat("b", 125)
	if e := writeFile(c, m, true); e != nil {
		t.Fatal(e)
	}
	duplicate := freshFile()
	duplicate.UploadRequestID = m.UploadRequestID
	expectFileSQLState(t, writeFile(c, duplicate, true), "23505")
	_, e := c.Exec(context.Background(), "SELECT file_metadata_filename_valid($1)", "a\x00b")
	expectFileSQLState(t, e, "22021")
	for _, s := range []string{"\ufeffx.pdf", "x y.pdf", "é.pdf"} {
		var ok bool
		if e := c.QueryRow(context.Background(), "SELECT file_metadata_filename_valid($1)", s).Scan(&ok); e != nil || !ok {
			t.Fatal(s, ok, e)
		}
	}
}
func TestFileFoundationMigrationEmptyDownUp(t *testing.T) {
	c := fileFoundationDB(t)
	seedBodyMessage(t, c)
	if e := fileMigration(t, c, "down"); e != nil {
		t.Fatal(e)
	}
	var missing bool
	if e := c.QueryRow(context.Background(), "SELECT to_regclass('file_objects') IS NULL AND to_regclass('file_lifecycle_events') IS NULL").Scan(&missing); e != nil || !missing {
		t.Fatal(missing, e)
	}
	if e := fileMigration(t, c, "up"); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := c.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	for _, index := range []string{"file_objects_conversation", "file_objects_pending", "file_lifecycle_events_version"} {
		var ok bool
		if e := c.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1)", index).Scan(&ok); e != nil || !ok {
			t.Fatal(index, ok, e)
		}
	}
	var reason string
	if e := c.QueryRow(context.Background(), "SELECT file_lifecycle_reason(NULL,'allocated')").Scan(&reason); e != nil || reason != "allocated" {
		t.Fatal(reason, e)
	}
}
func TestFileFoundationMigrationDownProtectsEvidence(t *testing.T) {
	for _, state := range []string{"allocated", "deleted"} {
		t.Run(state, func(t *testing.T) {
			c := fileFoundationDB(t)
			seedDirectConversation(t, c)
			storedFile(t, c, state)
			expectFileSQLState(t, fileMigration(t, c, "down"), "23514")
			run(t, c, "ROLLBACK")
			var n int
			if e := c.QueryRow(context.Background(), "SELECT count(*) FROM file_objects").Scan(&n); e != nil || n != 1 {
				t.Fatal(n, e)
			}
		})
	}
}

// Keep the input parameter formatter independent of SQL syntax errors.
func intString(n int) string { return strconv.Itoa(n) }
