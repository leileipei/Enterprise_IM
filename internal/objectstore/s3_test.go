package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

var location = Location{TenantID: "00000000-0000-4000-8000-000000000001", FileID: "00000000-0000-4000-8000-000000000002"}

const attemptID = "00000000-0000-4000-8000-000000000003"

func testS3(t *testing.T, h http.HandlerFunc) Store {
	t.Helper()
	t.Setenv("IM_FILE_S3_ACCESS_KEY", "test-access")
	t.Setenv("IM_FILE_S3_SECRET_KEY", "test-secret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, e := NewS3(Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "test-files", CredentialSource: "environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func measured(b []byte) files.Measurement {
	return files.Measurement{SizeBytes: int64(len(b)), SHA256: sha256.Sum256(b), DetectedMediaType: "text/plain"}
}
func TestS3VersionContract(t *testing.T) {
	b := []byte("hello")
	m := measured(b)
	var puts atomic.Int32
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts.Add(1)
			w.Header().Set("X-Amz-Version-Id", "version-1")
			io.Copy(io.Discard, r.Body)
			return
		}
		if r.URL.Query().Get("versionId") != "version-1" {
			t.Error("missing exact version")
		}
		w.Header().Set("X-Amz-Version-Id", "version-1")
		w.Header().Set("Etag", "not-a-sha")
		w.Write(b)
	})
	ref, e := s.PutVersion(context.Background(), location, attemptID, m, bytes.NewReader(b))
	if e != nil || ref.VersionID != "version-1" || puts.Load() != 1 {
		t.Fatal(ref, e, puts.Load())
	}
	body, e := s.ReadVersion(context.Background(), ref)
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(body)
	body.Close()
	if e != nil || !bytes.Equal(got, b) {
		t.Fatal(string(got), e)
	}
	m.SHA256 = [32]byte{}
	if _, e = s.PutVersion(context.Background(), location, attemptID, m, bytes.NewReader(b)); e == nil || puts.Load() != 1 {
		t.Fatal("unverified sha PUT", e)
	}
	if _, e = s.PutVersion(context.Background(), location, attemptID, measured(b), bytes.NewReader(append(b, '!'))); e == nil || puts.Load() != 1 {
		t.Fatal("extra byte PUT", e)
	}
	for _, v := range []string{"", "null", "v\n1"} {
		if _, e = s.ReadVersion(context.Background(), VersionRef{Location: location, VersionID: v}); e == nil {
			t.Fatal("invalid version", v)
		}
	}
}
func TestS3NoPutRetry(t *testing.T) {
	var requests atomic.Int32
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
	})
	b := []byte("written but response lost")
	if _, e := s.PutVersion(context.Background(), location, attemptID, measured(b), bytes.NewReader(b)); e == nil || requests.Load() != 1 {
		t.Fatal(e, requests.Load())
	}
}
func TestS3VersionMismatch(t *testing.T) {
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Version-Id", "different")
		w.Write([]byte("wrong"))
	})
	if _, e := s.ReadVersion(context.Background(), VersionRef{Location: location, VersionID: "fixed"}); e == nil {
		t.Fatal("substituted latest")
	}
}
func TestS3VersionReadbackTamper(t *testing.T) {
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Version-Id", "v1")
		if r.Method != "PUT" {
			w.Header().Set("ETag", "real-looking-but-wrong")
			w.Write([]byte("evil!"))
		}
	})
	b := []byte("hello")
	if _, e := s.PutVersion(context.Background(), location, attemptID, measured(b), bytes.NewReader(b)); e == nil {
		t.Fatal("ETag substituted for measured SHA")
	}
}
func TestS3BoundedRecovery(t *testing.T) {
	var pages atomic.Int32
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		n := pages.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>true</IsTruncated><NextKeyMarker>key-%d</NextKeyMarker><NextVersionIdMarker>version-%d</NextVersionIdMarker></ListVersionsResult>`, n, n)
	})
	if _, e := s.FindAttemptVersions(context.Background(), location, attemptID, 100); e == nil || pages.Load() > 10 {
		t.Fatal("unbounded or accepted truncated enumeration", e, pages.Load())
	}
}
