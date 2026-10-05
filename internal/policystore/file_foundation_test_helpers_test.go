package policystore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fileFixtureSeq atomic.Int64

func fileFoundationDB(t *testing.T) *pgx.Conn {
	t.Helper()
	c := db(t)

	return c
}
func expectFileSQLState(t *testing.T, e error, w string) {
	t.Helper()
	var p *pgconn.PgError
	if !errors.As(e, &p) || p.SQLState() != w {
		t.Fatalf("SQLSTATE want %s got %v", w, e)
	}
}
func freshFile() files.Metadata {
	id := fmt.Sprintf("10000000-0000-4000-8000-%012d", fileFixtureSeq.Add(1))
	p := files.CreateParams{TenantID: tenantA, ConversationID: directA, UploaderUserID: adminA, UploaderMembershipID: adminM, UploadRequestID: id, OriginalFilename: "报告.pdf", DeclaredMediaType: "application/pdf", DeclaredSizeBytes: 1}
	d, e := files.CreationDigest(p)
	if e != nil {
		panic(e)
	}
	return files.Metadata{CreateParams: p, ID: id, RequestDigest: d[:], State: "allocated", CreatedAt: at, UpdatedAt: at, UploadExpiresAt: at.Add(time.Hour)}
}
func nullableFileText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var fileColumns = []string{"id", "tenant_id", "conversation_id", "uploader_user_id", "uploader_membership_id", "upload_request_id", "request_digest", "original_filename", "declared_media_type", "declared_size_bytes", "state", "state_version", "created_at", "updated_at", "upload_expires_at", "object_key", "object_version_id", "detected_media_type", "actual_size_bytes", "sha256", "uploaded_at", "scan_job_id", "scan_engine", "scan_definition_version", "scanned_at", "scan_sha256", "deletion_requested_at", "deleted_at"}

func fileArgs(m files.Metadata) []any {
	return []any{m.ID, m.TenantID, m.ConversationID, m.UploaderUserID, m.UploaderMembershipID, m.UploadRequestID, m.RequestDigest, nullableFileText(m.OriginalFilename), nullableFileText(m.DeclaredMediaType), m.DeclaredSizeBytes, string(m.State), m.StateVersion, m.CreatedAt, m.UpdatedAt, m.UploadExpiresAt, nullableFileText(m.ObjectKey), nullableFileText(m.ObjectVersionID), nullableFileText(m.DetectedMediaType), m.ActualSizeBytes, m.SHA256, m.UploadedAt, nullableFileText(m.ScanJobID), nullableFileText(m.ScanEngine), nullableFileText(m.ScanDefinitionVersion), m.ScannedAt, m.ScanSHA256, m.DeletionRequestedAt, m.DeletedAt}
}
func writeFile(c *pgx.Conn, m files.Metadata, insert bool) error {
	parts := make([]string, len(fileColumns))
	for i, k := range fileColumns {
		if insert {
			parts[i] = fmt.Sprintf("$%d", i+1)
		} else {
			parts[i] = fmt.Sprintf("%s=$%d", k, i+1)
		}
	}
	q := "UPDATE file_objects SET " + strings.Join(parts, ",") + " WHERE id=$1"
	if insert {
		q = "INSERT INTO file_objects(" + strings.Join(fileColumns, ",") + ") VALUES(" + strings.Join(parts, ",") + ")"
	}
	_, e := c.Exec(context.Background(), q, fileArgs(m)...)
	return e
}
func fileNext(m files.Metadata, s string) files.Metadata {
	m.State = files.State(s)
	m.StateVersion++
	m.UpdatedAt = m.UpdatedAt.Add(time.Second)
	switch s {
	case "uploaded":
		m.ObjectKey = "tenants/" + m.TenantID + "/files/" + m.ID
		m.ObjectVersionID = "opaque-V1"
		m.DetectedMediaType = "application/pdf"
		x := m.DeclaredSizeBytes
		m.ActualSizeBytes = &x
		m.SHA256 = bytes.Repeat([]byte{0xab}, 32)
		a := m.UpdatedAt
		m.UploadedAt = &a
	case "scanning":
		job := "10000000-0000-4000-8000-000000000061"
		if m.ScanJobID != "" {
			job = "10000000-0000-4000-8000-000000000062"
		}
		m.ScanJobID = job
		m.ScanEngine = ""
		m.ScanDefinitionVersion = ""
		m.ScannedAt = nil
		m.ScanSHA256 = nil
	case "ready", "rejected":
		m.ScanEngine = "engine"
		m.ScanDefinitionVersion = "definitions-1"
		a := m.UpdatedAt
		m.ScannedAt = &a
		m.ScanSHA256 = bytes.Clone(m.SHA256)
	case "scan_failed":
		m.ScanEngine = ""
		m.ScanDefinitionVersion = ""
		m.ScannedAt = nil
		m.ScanSHA256 = nil
	case "delete_pending":
		a := m.UpdatedAt
		m.DeletionRequestedAt = &a
	case "deleted":
		a := m.UpdatedAt
		m.DeletedAt = &a
		m.OriginalFilename = ""
		m.DeclaredMediaType = ""
		m.ObjectKey = ""
		m.ObjectVersionID = ""
		m.DetectedMediaType = ""
		m.SHA256 = nil
		m.ScanJobID = ""
		m.ScanEngine = ""
		m.ScanDefinitionVersion = ""
		m.ScannedAt = nil
		m.ScanSHA256 = nil
	}
	return m
}
func storedFile(t *testing.T, c *pgx.Conn, s string) files.Metadata {
	t.Helper()
	m := freshFile()
	if e := writeFile(c, m, true); e != nil {
		t.Fatal(e)
	}
	path := []string{}
	switch s {
	case "uploaded":
		path = []string{"uploaded"}
	case "scanning":
		path = []string{"uploaded", "scanning"}
	case "ready", "rejected", "scan_failed":
		path = []string{"uploaded", "scanning", s}
	case "delete_pending":
		path = []string{"uploaded", "scanning", "ready", "delete_pending"}
	case "deleted":
		path = []string{"uploaded", "scanning", "ready", "delete_pending", "deleted"}
	}
	for _, s := range path {
		m = fileNext(m, s)
		if e := writeFile(c, m, false); e != nil {
			t.Fatal(e)
		}
	}
	return m
}
func fileMigration(t *testing.T, c *pgx.Conn, which string) error {
	t.Helper()
	b, e := os.ReadFile("../../db/migrations/000018_file_foundation." + which + ".sql")
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.PgConn().Exec(context.Background(), string(b)).ReadAll()
	return e
}
func filePeer(t *testing.T, c *pgx.Conn) *pgx.Conn {
	t.Helper()
	p, e := pgx.Connect(context.Background(), os.Getenv("IM_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	var schema string
	if e = c.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	run(t, p, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()+",public")
	t.Cleanup(func() { p.Exec(context.Background(), "ROLLBACK"); p.Close(context.Background()) })
	return p
}
