package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/files"
)

func readOnlyHTTP(t *testing.T, h http.HandlerFunc) ReadOnlyStore {
	t.Helper()
	t.Setenv("IM_FILE_DOWNLOAD_S3_ACCESS_KEY", "download-key")
	t.Setenv("IM_FILE_DOWNLOAD_S3_SECRET_KEY", "download-secret")
	t.Setenv("IM_FILE_S3_ACCESS_KEY", "upload-key")
	t.Setenv("IM_FILE_S3_SECRET_KEY", "upload-secret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, e := NewS3ReadOnly(Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "test-files", PathStyle: true, CredentialSource: "download_environment"})
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestS3ReadOnlyCredentials(t *testing.T) {
	s := readOnlyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=download-key/") || strings.Contains(r.Header.Get("Authorization"), "upload-key") {
			t.Error("reader used wrong signing principal")
		}
		if r.Method != "GET" || r.URL.Query().Get("versionId") != "v1" {
			t.Error("not a fixed version read")
		}
		w.Header().Set("X-Amz-Version-Id", "v1")
		io.WriteString(w, "hello")
	})
	body, e := s.ReadVersion(context.Background(), VersionRef{Location: location, VersionID: "v1"})
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(body)
	body.Close()
	if e != nil || string(got) != "hello" {
		t.Fatal("fixed bytes not read", e)
	}
	c := Config{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "test-files", CredentialSource: "download_environment"}
	for _, key := range []string{"IM_FILE_DOWNLOAD_S3_ACCESS_KEY", "IM_FILE_DOWNLOAD_S3_SECRET_KEY"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			if _, e := NewS3ReadOnly(c); e == nil {
				t.Fatal("uploader fallback accepted")
			}
		})
	}
	c.CredentialSource = "environment"
	if _, e := NewS3ReadOnly(c); e == nil {
		t.Fatal("arbitrary credential role accepted")
	}
}

func TestS3ReadOnlyMutationsDenied(t *testing.T) {
	var requests atomic.Int32
	s := readOnlyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("read-only mutation contacted storage")
	})
	if _, e := s.PutVersion(context.Background(), location, attemptID, measured([]byte("hello")), bytes.NewReader([]byte("hello"))); !errors.Is(e, files.ErrDependencyUnavailable) {
		t.Fatal("PUT not explicitly rejected", e)
	}
	if _, e := s.FindAttemptVersions(context.Background(), location, attemptID, 100); !errors.Is(e, files.ErrDependencyUnavailable) {
		t.Fatal("version enumeration not rejected", e)
	}
	if requests.Load() != 0 {
		t.Fatal("read-only method caused network I/O")
	}
}
