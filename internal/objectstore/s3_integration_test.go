package objectstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func integrationS3(t *testing.T) Store {
	t.Helper()
	if os.Getenv("IM_TEST_S3_ENDPOINT") == "" {
		t.Skip("requires dedicated S3; scripts/test-file-runtime.sh prechecks it")
	}
	s, e := NewS3(Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Region: "us-east-1", Bucket: os.Getenv("IM_TEST_S3_BUCKET"), CredentialSource: "environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestS3PrivateIntegration(t *testing.T) {
	s := integrationS3(t)
	ctx := context.Background()
	if e := s.ValidateCapabilities(ctx); e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	buf[6] = 0x40
	buf[8] = 0x80
	h := hex.EncodeToString(buf)
	loc := location
	loc.FileID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	old := []byte("first fixed version")
	a, e := s.PutVersion(ctx, loc, attemptID, measured(old), bytes.NewReader(old))
	if e != nil {
		t.Fatal(e)
	}
	next := []byte("new version")
	b, e := s.PutVersion(ctx, loc, attemptID, measured(next), bytes.NewReader(next))
	if e != nil || a.VersionID == b.VersionID {
		t.Fatal(a, b, e)
	}
	body, e := s.ReadVersion(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(body)
	body.Close()
	if e != nil || !bytes.Equal(got, old) {
		t.Fatal("fixed version replaced", e)
	}
	r, e := http.Get(os.Getenv("IM_TEST_S3_ENDPOINT") + "/" + os.Getenv("IM_TEST_S3_BUCKET") + "/tenants/" + loc.TenantID + "/files/" + loc.FileID)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal("anonymous object access", r.StatusCode)
	}
	refs, e := s.FindAttemptVersions(ctx, loc, attemptID, 100)
	if e != nil || len(refs) != 2 {
		t.Fatal(refs, e)
	}
}

func TestS3PrivatePolicyIntegration(t *testing.T) {
	bucketName := os.Getenv("IM_TEST_S3_POLICY_BUCKET")
	if bucketName == "" {
		t.Skip("dedicated policy fixture bucket required")
	}
	if bucketName == os.Getenv("IM_TEST_S3_BUCKET") {
		t.Fatal("policy fixture must be isolated")
	}
	store, e := NewS3(Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Region: "us-east-1", Bucket: bucketName, CredentialSource: "environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	s := store.(*s3Store)
	ctx := context.Background()
	bucket := aws.String(bucketName)
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + *bucket + `/*"]}]}`
	if _, e := s.client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: bucket, Policy: aws.String(policy)}); e != nil {
		t.Fatal("fixture policy setup failed")
	}
	defer func() {
		if _, e := s.client.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: bucket}); e != nil {
			t.Error("fixture policy cleanup failed")
		}
	}()
	if e := store.ValidateCapabilities(ctx); e == nil {
		t.Fatal("anonymous grant accepted")
	}
}
func TestS3NoPutRetryAfterRealWrite(t *testing.T) {
	origin := integrationS3(t)
	endpoint, _ := url.Parse(os.Getenv("IM_TEST_S3_ENDPOINT"))
	var puts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Scheme = endpoint.Scheme
		r.URL.Host = endpoint.Host
		r.RequestURI = ""
		resp, e := http.DefaultTransport.RoundTrip(r)
		if e != nil {
			t.Error("fixture proxy failed")
			w.WriteHeader(503)
			return
		}
		defer resp.Body.Close()
		if r.Method == "PUT" {
			puts.Add(1)
			if resp.StatusCode != 200 {
				t.Error("real write failed", resp.StatusCode)
			}
			io.Copy(io.Discard, resp.Body)
			w.WriteHeader(503)
			return
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	store, e := NewS3(Config{Endpoint: proxy.URL, Region: "us-east-1", Bucket: os.Getenv("IM_TEST_S3_BUCKET"), CredentialSource: "environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	loc := location
	loc.FileID = "00000000-0000-4000-8000-" + strings.Repeat("a", 12)
	body := []byte("one real write despite lost response")
	if _, e = store.PutVersion(context.Background(), loc, attemptID, measured(body), bytes.NewReader(body)); e == nil || puts.Load() != 1 {
		t.Fatal(e, puts.Load())
	}
	refs, e := origin.FindAttemptVersions(context.Background(), loc, attemptID, 100)
	if e != nil || len(refs) < 1 {
		t.Fatal("uncertain real write not recoverable", e)
	}
}
