package objectstore

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3ReadProbeStrict(t *testing.T) {
	for _, tc := range []struct {
		name, requestVersion, responseVersion, content string
		status                                         int
		ok                                             bool
	}{
		{"valid", "probe-v1", "probe-v1", "enterprise-im-file-read-probe-v1\n", 200, true},
		{"missing_lf", "probe-v1", "probe-v1", "enterprise-im-file-read-probe-v1", 200, false},
		{"extra_byte", "probe-v1", "probe-v1", "enterprise-im-file-read-probe-v1\nx", 200, false},
		{"large", "probe-v1", "probe-v1", strings.Repeat("x", 1025), 200, false},
		{"wrong_version", "probe-v1", "other", "enterprise-im-file-read-probe-v1\n", 200, false},
		{"missing_version", "probe-v1", "", "enterprise-im-file-read-probe-v1\n", 200, false},
		{"null_requested", "null", "null", "enterprise-im-file-read-probe-v1\n", 200, false},
		{"empty_requested", "", "", "enterprise-im-file-read-probe-v1\n", 200, false},
		{"denied", "probe-v1", "", "", 403, false},
		{"missing", "probe-v1", "", "", 404, false},
		{"redirect", "probe-v1", "", "", 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			s := readOnlyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != "GET" || r.URL.Path != "/test-files/_im_runtime/read-probe/v1" || r.URL.Query().Get("versionId") != tc.requestVersion {
					t.Error("probe request escaped fixed read contract")
				}
				w.Header().Set("X-Amz-Version-Id", tc.responseVersion)
				if tc.status == 302 {
					w.Header().Set("Location", "http://127.0.0.1:1/leaked")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.content)
			})
			if e := s.ValidateReadProbe(context.Background(), tc.requestVersion); (e == nil) != tc.ok {
				t.Fatal("probe acceptance mismatch", e)
			}
			if tc.requestVersion == "" || tc.requestVersion == "null" {
				if requests != 0 {
					t.Fatal("invalid version contacted storage")
				}
			} else if requests != 1 {
				t.Fatal("unexpected retry or redirect", requests)
			}
		})
	}
	t.Run("bounded_body_closed", func(t *testing.T) {
		body := &probeBody{Reader: strings.NewReader(strings.Repeat("x", 2048))}
		out := &s3.GetObjectOutput{Body: body, VersionId: aws.String("probe-v1")}
		if validateReadProbeResponse(out, "probe-v1") == nil || body.read > 1024 || body.closes != 1 {
			t.Fatal("unbounded or unclosed body", body.read, body.closes)
		}
		mismatch := &probeBody{Reader: strings.NewReader("enterprise-im-file-read-probe-v1\n")}
		if validateReadProbeResponse(&s3.GetObjectOutput{Body: mismatch, VersionId: aws.String("different")}, "probe-v1") == nil || mismatch.closes != 1 {
			t.Fatal("mismatched response leaked body")
		}
	})
}

type probeBody struct {
	*strings.Reader
	read, closes int
}

func (b *probeBody) Read(p []byte) (int, error) { n, e := b.Reader.Read(p); b.read += n; return n, e }
func (b *probeBody) Close() error               { b.closes++; return nil }

func TestS3ReadProbeBudget(t *testing.T) {
	s := readOnlyHTTP(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	before := time.Now()
	if e := s.ValidateReadProbe(ctx, "probe-v1"); e == nil {
		t.Fatal("deadline ignored")
	}
	if time.Since(before) > time.Second {
		t.Fatal("probe refreshed caller budget")
	}
}
