package objectstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestS3VersionDeleteIntegration(t *testing.T) {
	upload := integrationS3(t)
	if os.Getenv("IM_FILE_CLEANUP_S3_ACCESS_KEY") == "" || os.Getenv("IM_FILE_CLEANUP_S3_SECRET_KEY") == "" {
		t.Fatal("dedicated cleanup role required")
	}
	c := Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Bucket: os.Getenv("IM_TEST_S3_BUCKET"), Region: "us-east-1", CredentialSource: "cleanup_environment", PathStyle: true}
	d, e := NewS3VersionDeleter(c)
	if e != nil {
		t.Fatal("cleanup adapter unavailable")
	}
	raw := make([]byte, 16)
	if _, e = rand.Read(raw); e != nil {
		t.Fatal(e)
	}
	raw[6] = (raw[6] & 15) | 64
	raw[8] = (raw[8] & 63) | 128
	h := hex.EncodeToString(raw)
	loc := location
	loc.FileID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	ctx := context.Background()
	data := []byte("owned p4-24 version deletion fixture")
	a, e := upload.PutVersion(ctx, loc, attemptID, measured(data), bytes.NewReader(data))
	if e != nil {
		t.Fatal("owned upload failed")
	}
	defer d.DeleteVersion(ctx, a)
	b, e := upload.PutVersion(ctx, loc, attemptID, measured(data), bytes.NewReader(data))
	if e != nil {
		t.Fatal("owned second version failed")
	}
	defer d.DeleteVersion(ctx, b)
	page, e := d.ListVersions(ctx, loc, VersionCursor{}, 100)
	if e != nil || !page.Exhausted || len(page.Versions) != 2 {
		t.Fatal("actual inventory failed", e)
	}
	p, e := d.ProbeVersion(ctx, a)
	if e != nil || p != VersionPresent {
		t.Fatal("actual fixed probe failed", p, e)
	}
	// The narrow identity cannot upload or perform unversioned deletion.
	client := d.(*s3VersionDeleter).client
	key, _ := loc.key()
	_, e = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
	var api smithy.APIError
	if !errors.As(e, &api) || api.ErrorCode() != "AccessDenied" {
		t.Fatal("cleanup PUT was not denied")
	}
	for _, version := range []*string{nil, aws.String(""), aws.String("null")} {
		_, e = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), VersionId: version})
		var denial smithy.APIError
		if !errors.As(e, &denial) || denial.ErrorCode() != "AccessDenied" {
			t.Fatal("unversioned, empty or null DELETE was not denied")
		}
	}
	if e = d.DeleteVersion(ctx, a); e != nil {
		_, de := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), VersionId: aws.String(a.VersionID)})
		code := "unclassified"
		var detail smithy.APIError
		if errors.As(de, &detail) {
			code = detail.ErrorCode()
		}
		t.Fatal("actual fixed deletion failed", code)
	}
	p, e = d.ProbeVersion(ctx, a)
	if e != nil || p != VersionAbsent {
		out, probeErr := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), VersionId: aws.String(a.VersionID)})
		if out != nil && out.Body != nil {
			out.Body.Close()
		}
		code := "unclassified"
		if errors.As(probeErr, &api) {
			code = api.ErrorCode()
		}
		t.Fatal("actual absence proof failed", p, "provider_code", code)
	}
	p, e = d.ProbeVersion(ctx, b)
	if e != nil || p != VersionPresent {
		t.Fatal("other version was affected", p, e)
	}
}
