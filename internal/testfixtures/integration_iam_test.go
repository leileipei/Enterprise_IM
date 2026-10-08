package testfixtures

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5"
)

// This gate deliberately fails on missing fixtures; a skipped matrix is not evidence.
func TestIntegrationFixtureIAM(t *testing.T) {
	required := []string{"IM_TEST_DATABASE_URL", "IM_TEST_S3_ENDPOINT", "IM_TEST_S3_BUCKET", "IM_TEST_S3_POLICY_BUCKET", "IM_TEST_INTEGRATION_PROBE_VERSION"}
	for _, key := range required {
		if os.Getenv(key) == "" {
			t.Fatal("integration fixture missing: " + key)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Run("postgres_tls_and_wrong_ca", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, os.Getenv("IM_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal("actual host PostgreSQL TLS connection failed")
		}
		defer conn.Close(ctx)
		var tls bool
		if err = conn.QueryRow(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()").Scan(&tls); err != nil || !tls {
			t.Fatal("PostgreSQL session has no TLS")
		}
		u, err := url.Parse(os.Getenv("IM_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal("fixture database URL invalid")
		}
		q := u.Query()
		ca := q.Get("sslrootcert")
		if ca == "" {
			t.Fatal("fixture CA missing")
		}
		q.Set("sslrootcert", strings.TrimSuffix(ca, "ca.crt")+"wrong-ca.crt")
		u.RawQuery = q.Encode()
		wrong, err := pgx.Connect(ctx, u.String())
		if err == nil {
			wrong.Close(ctx)
			t.Fatal("wrong CA accepted")
		}
		var ordinary int
		err = conn.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname IN ('p431_api','p431_repair','p431_import_writer') AND NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb").Scan(&ordinary)
		if err != nil || ordinary != 3 {
			t.Fatal("ordinary product database role matrix unproven")
		}
	})
	endpoint, bucket := os.Getenv("IM_TEST_S3_ENDPOINT"), os.Getenv("IM_TEST_S3_BUCKET")
	if !strings.HasPrefix(bucket, "p426-p431-") || os.Getenv("IM_TEST_S3_POLICY_BUCKET") != bucket+"-policy" {
		t.Fatal("dedicated bucket ownership invalid")
	}
	clients := map[string]*s3.Client{}
	keys := map[string]bool{}
	for _, role := range []string{"UPLOAD", "WORKER", "DOWNLOAD", "CLEANUP", "BOOTSTRAP"} {
		prefix := "IM_TEST_FILE_" + role + "_"
		if role == "CLEANUP" {
			prefix = "IM_FILE_CLEANUP_S3_"
		}
		key, secret := os.Getenv(prefix+"ACCESS_KEY"), os.Getenv(prefix+"SECRET_KEY")
		if key == "" || secret == "" || keys[key] {
			t.Fatal("distinct IAM principals missing")
		}
		keys[key] = true
		config := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(key, secret, ""), RetryMaxAttempts: 1}
		clients[role] = s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
	}
	denied := func(t *testing.T, err error) {
		t.Helper()
		var api smithy.APIError
		if err == nil || !errors.As(err, &api) || api.ErrorCode() != "AccessDenied" {
			t.Fatal("operation was not denied by IAM")
		}
	}
	key := "tenants/p431/files/" + fmt.Sprintf("iam-%d", time.Now().UnixNano())
	bootstrap := clients["BOOTSTRAP"]
	t.Run("buckets_versioned_and_probe_fixed", func(t *testing.T) {
		for _, name := range []string{bucket, bucket + "-policy"} {
			out, err := bootstrap.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(name)})
			if err != nil || string(out.Status) != "Enabled" {
				t.Fatal("fixture bucket versioning disabled")
			}
		}
		version := os.Getenv("IM_TEST_INTEGRATION_PROBE_VERSION")
		if version == "null" {
			t.Fatal("probe has null version")
		}
		out, err := clients["DOWNLOAD"].GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("_im_runtime/read-probe/v1"), VersionId: aws.String(version)})
		if err != nil {
			t.Fatal("fixed probe download failed")
		}
		data, err := io.ReadAll(out.Body)
		out.Body.Close()
		if err != nil || string(data) != "enterprise-im-file-read-probe-v1\n" || aws.ToString(out.VersionId) != version {
			t.Fatal("fixed probe content/version mismatch")
		}
		_, err = clients["UPLOAD"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("_im_runtime/read-probe/v1"), Body: strings.NewReader("foreign")})
		denied(t, err)
	})
	var first, second string
	t.Run("upload_allowed_and_versioned", func(t *testing.T) {
		for i, body := range []string{"first", "second"} {
			out, err := clients["UPLOAD"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body)})
			if err != nil || aws.ToString(out.VersionId) == "" || aws.ToString(out.VersionId) == "null" {
				t.Fatal("versioned upload failed")
			}
			if i == 0 {
				first = aws.ToString(out.VersionId)
			} else {
				second = aws.ToString(out.VersionId)
			}
		}
		if first == second {
			t.Fatal("object versions not distinct")
		}
		_, err := clients["UPLOAD"].DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(first)})
		denied(t, err)
		_, err = clients["UPLOAD"].DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		denied(t, err)
	})
	if first == "" || second == "" {
		t.Fatal("matrix cannot proceed without uploaded versions")
	}
	t.Run("anonymous_denied", func(t *testing.T) {
		for _, role := range []string{"UPLOAD", "WORKER", "DOWNLOAD", "CLEANUP"} {
			for _, foreign := range []string{"p426-p431-00000000000000000000000000000000", bucket + "-policy"} {
				_, err := clients[role].GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(foreign)})
				denied(t, err)
				_, err = clients[role].GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(foreign), Key: aws.String(key), VersionId: aws.String(first)})
				denied(t, err)
				_, err = clients[role].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(foreign), Key: aws.String(key), Body: strings.NewReader("forbidden")})
				denied(t, err)
			}
		}
		client := http.Client{Timeout: 10 * time.Second}
		response, err := client.Get(endpoint + "/" + bucket + "/" + key)
		if err != nil {
			t.Fatal("anonymous request failed to reach fixture")
		}
		defer response.Body.Close()
		if response.StatusCode != 403 {
			t.Fatal("anonymous object access accepted")
		}
	})
	for _, role := range []string{"WORKER", "DOWNLOAD"} {
		t.Run(strings.ToLower(role)+"_read_only", func(t *testing.T) {
			client := clients[role]
			out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(first)})
			if err != nil {
				t.Fatal("allowed exact version read failed")
			}
			data, err := io.ReadAll(out.Body)
			out.Body.Close()
			if err != nil || string(data) != "first" {
				t.Fatal("exact version read mismatch")
			}
			_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("forbidden")})
			denied(t, err)
			_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(first)})
			denied(t, err)
			if role == "DOWNLOAD" {
				_, err = client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
				denied(t, err)
			} else {
				versions, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
				if err != nil || len(versions.Versions) != 2 {
					t.Fatal("worker version enumeration failed")
				}
			}
		})
	}
	t.Run("cleanup_rejects_nil_empty_null_versions", func(t *testing.T) {
		_, err := clients["CLEANUP"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("forbidden")})
		denied(t, err)
		for _, version := range []*string{nil, aws.String(""), aws.String("null")} {
			_, err := clients["CLEANUP"].DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: version})
			denied(t, err)
		}
	})
	t.Run("cleanup_exact_version_only", func(t *testing.T) {
		_, err := clients["CLEANUP"].DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(first)})
		if err != nil {
			t.Fatal("allowed exact version cleanup failed")
		}
		_, err = bootstrap.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(first)})
		var api smithy.APIError
		if err == nil || !errors.As(err, &api) || (api.ErrorCode() != "NoSuchVersion" && api.ErrorCode() != "NoSuchKey") {
			t.Fatal("deleted version still exists")
		}
		out, err := bootstrap.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(second)})
		if err != nil {
			t.Fatal("cleanup removed another version")
		}
		out.Body.Close()
	})
}
